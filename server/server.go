package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenk/backoff"
	"github.com/gorilla/mux"
	"github.com/jtblin/kube2iam"
	"github.com/jtblin/kube2iam/iam"
	"github.com/jtblin/kube2iam/k8s"
	"github.com/jtblin/kube2iam/mappings"
	"github.com/jtblin/kube2iam/metrics"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/cache"

	_ "net/http/pprof"
)

const (
	defaultAppPort                    = "8181"
	defaultCacheSyncAttempts          = 10
	defaultIAMRoleKey                 = "iam.amazonaws.com/role"
	defaultIAMExternalID              = "iam.amazonaws.com/external-id"
	defaultLogLevel                   = "info"
	defaultLogFormat                  = "text"
	defaultMaxElapsedTime             = 1 * time.Second
	defaultIAMRoleSessionTTL          = 15 * time.Minute
	defaultMaxInterval                = 1 * time.Second
	defaultMetadataAddress            = "169.254.169.254"
	defaultNamespaceKey               = "iam.amazonaws.com/allowed-roles"
	defaultCacheResyncPeriod          = 30 * time.Minute
	defaultResolveDupIPs              = false
	defaultNamespaceRestrictionFormat = "glob"
	healthcheckInterval               = 30 * time.Second
	defaultStsVpcEndpoint             = ""
)

var tokenRouteRegexp = regexp.MustCompile("^/?[^/]+/api/token$")

// Keeps track of the names of registered handlers for metric value/label initialization
var registeredHandlerNames []string

// Server encapsulates all of the parameters necessary for starting up
// the server. These can either be set via command line or directly.
type Server struct {
	APIServer                  string
	APIToken                   string
	AppPort                    string
	MetricsPort                string
	BaseRoleARN                string
	DefaultIAMRole             string
	IAMRoleKey                 string
	IAMExternalID              string
	IAMRoleSessionTTL          time.Duration
	MetadataAddress            string
	HostInterface              string
	HostIP                     string
	NodeName                   string
	NamespaceKey               string
	CacheResyncPeriod          time.Duration
	LogLevel                   string
	LogFormat                  string
	NamespaceRestrictionFormat string
	ResolveDupIPs              bool
	UseRegionalStsEndpoint     bool
	AddIPTablesRule            bool
	AutoDiscoverBaseArn        bool
	AutoDiscoverDefaultRole    bool
	Debug                      bool
	Insecure                   bool
	NamespaceRestriction       bool
	Verbose                    bool
	Version                    bool
	iam                        *iam.Client
	k8s                        *k8s.Client
	roleMapper                 *mappings.RoleMapper
	BackoffMaxElapsedTime      time.Duration
	BackoffMaxInterval         time.Duration
	InstanceID                 string
	HealthcheckFailReason      string
	healthcheckTicker          *time.Ticker
	StsVpcEndPoint             string
	BootAsWebServer            bool
	BootAsWatcher              bool
}

type appHandlerFunc func(*log.Entry, http.ResponseWriter, *http.Request)

type appHandler struct {
	name string
	fn   appHandlerFunc
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{w, http.StatusOK}
}

// ServeHTTP implements the net/http server Handler interface
// and recovers from panics.
func (h *appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.WithFields(log.Fields{
		"req.method": r.Method,
		"req.path":   r.URL.Path,
		"req.remote": parseRemoteAddr(r.RemoteAddr),
	})
	rw := newResponseWriter(w)

	// Set up a prometheus timer to track the request duration. It returns the timer value when
	// observed and stores it in timeSecs to report in logs. A function polls the Request and responseWriter
	// for the correct labels at observation time.
	var timeSecs float64
	lvsProducer := func() []string {
		return []string{strconv.Itoa(rw.statusCode), r.Method, h.name}
	}
	timer := metrics.NewFunctionTimer(metrics.HTTPRequestSec, lvsProducer, &timeSecs)

	defer func() {
		var err error
		if rec := recover(); rec != nil {
			switch t := rec.(type) {
			case string:
				err = errors.New(t)
			case error:
				err = t
			default:
				err = errors.New("unknown error")
			}
			logger.WithField("res.status", http.StatusInternalServerError).
				Errorf("PANIC error processing request: %+v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}()
	h.fn(logger, rw, r)
	timer.ObserveDuration()
	latencyMilliseconds := timeSecs * 1e3
	if r.URL.Path != "/healthz" {
		logger.WithFields(log.Fields{"res.duration": latencyMilliseconds, "res.status": rw.statusCode}).
			Infof("%s %s (%d) took %f ms", r.Method, r.URL.Path, rw.statusCode, latencyMilliseconds)
	}
}

func newAppHandler(name string, fn appHandlerFunc) *appHandler {
	registeredHandlerNames = append(registeredHandlerNames, name)
	return &appHandler{name: name, fn: fn}
}

func parseRemoteAddr(addr string) string {
	n := strings.IndexByte(addr, ':')
	if n <= 1 {
		return ""
	}
	hostname := addr[0:n]
	if net.ParseIP(hostname) == nil {
		return ""
	}
	return hostname
}

func (s *Server) getRoleMapping(IP string) (*mappings.RoleMappingResult, error) {
	var roleMapping *mappings.RoleMappingResult
	var err error

	roleMapping, err = s.roleMapper.GetRoleMappingUsingCache(IP)

	if err != nil {
		return nil, err
	}

	return roleMapping, nil
}

func (s *Server) getExternalIDMapping(IP string) (string, error) {
	var externalID string
	var err error
	operation := func() error {
		externalID, err = s.roleMapper.GetExternalIDMapping(IP)
		return err
	}

	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.MaxInterval = s.BackoffMaxInterval
	expBackoff.MaxElapsedTime = s.BackoffMaxElapsedTime

	err = backoff.Retry(operation, expBackoff)
	if err != nil {
		return "", err
	}

	return externalID, nil
}

func (s *Server) beginPollHealthcheck(interval time.Duration) {
	if s.healthcheckTicker == nil {
		s.doHealthcheck()
		s.healthcheckTicker = time.NewTicker(interval)
		go func() {
			for {
				<-s.healthcheckTicker.C
				s.doHealthcheck()
			}
		}()
	}
}

func (s *Server) doHealthcheck() {
	// Track the healthcheck status as a metric value. Running this function in the background on a timer
	// allows us to update both the /healthz endpoint and healthcheck metric value at once and keep them in sync.
	var err error
	var errMsg string
	// This deferred function stores the reason for failure in a Server struct member by parsing the error object
	// produced during the healthcheck, if any. It also stores a different metric value for the healthcheck depending
	// on whether it passed or failed.
	defer func() {
		var healthcheckResult float64 = 1
		s.HealthcheckFailReason = errMsg // Is empty if no error
		if err != nil || len(errMsg) > 0 {
			healthcheckResult = 0
		}
		metrics.HealthcheckStatus.Set(healthcheckResult)
	}()

	resp, err := http.Get(fmt.Sprintf("http://%s/latest/meta-data/instance-id", s.MetadataAddress))
	if err != nil {
		errMsg = fmt.Sprintf("Error getting instance id %+v", err)
		log.Errorf(errMsg)
		return
	}
	if resp.StatusCode != 200 {
		errMsg = fmt.Sprintf("Error getting instance id, got status: %+s", resp.Status)
		log.Error(errMsg)
		return
	}
	defer resp.Body.Close()
	instanceID, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errMsg = fmt.Sprintf("Error reading response body %+v", err)
		log.Errorf(errMsg)
		return
	}
	s.InstanceID = string(instanceID)
}

// HealthResponse represents a response for the health check.
type HealthResponse struct {
	HostIP     string `json:"hostIP"`
	InstanceID string `json:"instanceId"`
}

func (s *Server) healthHandler(logger *log.Entry, w http.ResponseWriter, r *http.Request) {
	// healthHandler reports the last result of a timed healthcheck that repeats in the background.
	// The healthcheck logic is performed in doHealthcheck and saved into Server struct fields.
	// This "caching" of results allows the healthcheck to be monitored at a high request rate by external systems
	// without fear of overwhelming any rate limits with AWS or other dependencies.
	if len(s.HealthcheckFailReason) > 0 {
		http.Error(w, s.HealthcheckFailReason, http.StatusInternalServerError)
		return
	}

	health := &HealthResponse{InstanceID: s.InstanceID, HostIP: s.HostIP}
	w.Header().Add("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(health); err != nil {
		log.Errorf("Error sending json %+v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) debugStoreHandler(logger *log.Entry, w http.ResponseWriter, r *http.Request) {
	o, err := json.Marshal(s.roleMapper.DumpDebugInfo())
	if err != nil {
		log.Errorf("Error converting debug map to json: %+v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	write(logger, w, string(o))
}

func (s *Server) securityCredentialsHandler(logger *log.Entry, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "EC2ws")
	remoteIP := parseRemoteAddr(r.RemoteAddr)
	roleMapping, err := s.getRoleMapping(remoteIP)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If a base ARN has been supplied and this is not cross-account then
	// return a simple role-name, otherwise return the full ARN
	if s.iam.BaseARN != "" && strings.HasPrefix(roleMapping.Role, s.iam.BaseARN) {
		write(logger, w, strings.TrimPrefix(roleMapping.Role, s.iam.BaseARN))
		return
	}
	write(logger, w, roleMapping.Role)
}

func (s *Server) roleHandler(w http.ResponseWriter, r *http.Request) {
	bAssuemRoleStart := time.Now()

	w.Header().Set("Server", "EC2ws")

	remoteIP := parseRemoteAddr(r.RemoteAddr)
	wantedRole := mux.Vars(r)["role"]
	wantedRoleARN := s.iam.RoleARN(wantedRole)

	roleLogger := logrus.WithFields(log.Fields{
		"pod.iam.role": wantedRole,
	})

	roleLogger.Debugf("total time taken to call the assume role func %v", time.Since(bAssuemRoleStart).Milliseconds())

	credentials, err := s.iam.AssumeRole(wantedRoleARN, "", remoteIP, s.IAMRoleSessionTTL)
	if err != nil {
		roleLogger.Errorf("Error assuming role %+v", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	roleLogger.Debugf("retrieved credentials from sts endpoint: %s", s.iam.Endpoint)

	bJsonEncode := time.Now()
	if err := json.NewEncoder(w).Encode(credentials); err != nil {
		roleLogger.Errorf("Error sending json %+v", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}

	roleLogger.Debugf("total time taken to write json response %v", time.Since(bJsonEncode).Milliseconds())
}

func (s *Server) reverseProxyHandler(logger *log.Entry, w http.ResponseWriter, r *http.Request) {
	// Remove remoteaddr to prevent issues with new IMDSv2 to fail when x-forwarded-for header is present
	// for more details please see: https://github.com/aws/aws-sdk-ruby/issues/2177 https://github.com/uswitch/kiam/issues/359
	token := r.Header.Get("X-aws-ec2-metadata-token")
	if (r.Method == http.MethodPut && tokenRouteRegexp.MatchString(r.URL.Path)) || (r.Method == http.MethodGet && token != "") {
		r.RemoteAddr = ""
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: s.MetadataAddress})
	proxy.ServeHTTP(w, r)
	logger.WithField("metadata.url", s.MetadataAddress).Debug("Proxy ec2 metadata request")
}

func write(logger *log.Entry, w http.ResponseWriter, s string) {
	if _, err := w.Write([]byte(s)); err != nil {
		logger.Errorf("Error writing response: %+v", err)
	}
}

// Run runs the specified Server.
func (s *Server) Run(host, token, nodeName string, insecure bool) error {
	k, err := k8s.NewClient(host, token, nodeName, insecure, s.ResolveDupIPs)
	if err != nil {
		return err
	}

	s.k8s = k
	var nErr error
	s.iam, nErr = iam.NewClient(s.BaseRoleARN, s.UseRegionalStsEndpoint, s.StsVpcEndPoint)
	if nErr != nil {
		return nErr
	}

	s.roleMapper = mappings.NewRoleMapper(s.IAMRoleKey, s.IAMExternalID, s.DefaultIAMRole, s.NamespaceRestriction,
		s.NamespaceKey, s.iam, s.k8s, s.NamespaceRestrictionFormat)

	if s.BootAsWatcher {
		wg := new(sync.WaitGroup)
		wg.Add(1)

		go func() {
			log.Debugf("Starting pod and namespace sync jobs with %s resync period", s.CacheResyncPeriod.String())
			podSynched := s.k8s.WatchForPods(
				kube2iam.NewPodHandler(s.IAMRoleKey, s.DefaultIAMRole, s.NamespaceKey, s.iam), s.CacheResyncPeriod)
			namespaceSynched := s.k8s.WatchForNamespaces(kube2iam.NewNamespaceHandler(s.NamespaceKey), s.CacheResyncPeriod)

			synced := false
			for i := 0; i < defaultCacheSyncAttempts && !synced; i++ {
				synced = cache.WaitForCacheSync(nil, podSynched, namespaceSynched)
			}

			if !synced {
				log.Fatalf("Attempted to wait for caches to be synced for %d however it is not done.  Giving up.", defaultCacheSyncAttempts)
			} else {
				log.Debugln("Caches have been synced.  Proceeding with server.")
			}
		}()

		wg.Wait()
	} else if s.BootAsWebServer {

		r := mux.NewRouter()
		r.Path("/debug/pprof/trace").HandlerFunc(pprof.Trace)
		r.PathPrefix("/debug/pprof/").HandlerFunc(pprof.Index)

		securityHandler := newAppHandler("securityCredentialsHandler", s.securityCredentialsHandler)
		if s.Debug {
			// This is a potential security risk if enabled in some clusters, hence the flag
			r.Handle("/debug/store", newAppHandler("debugStoreHandler", s.debugStoreHandler))
		}
		r.Handle("/{version}/meta-data/iam/security-credentials", securityHandler)
		r.Handle("/{version}/meta-data/iam/security-credentials/", securityHandler)

		app, err := newrelic.NewApplication(
			newrelic.ConfigAppName("kube2iamweb"),
			newrelic.ConfigDistributedTracerEnabled(true),
			newrelic.ConfigLicense("41499b068d1ca57f539cfb044bd9ad144000b9b9"),
		)

		if err != nil {
			logrus.Debugf("failed to create application with newrelic %v", err.Error())

			return err
		} else {
			r.HandleFunc(newrelic.WrapHandleFunc(app, "/{version}/meta-data/iam/security-credentials/{role:.*}",
				s.roleHandler))
		}

		r.Handle("/healthz", newAppHandler("healthHandler", s.healthHandler))

		// This has to be registered last so that it catches fall-throughs
		r.Handle("/{path:.*}", newAppHandler("reverseProxyHandler", s.reverseProxyHandler))

		log.Infof("Listening on port %s", s.AppPort)
		if err := http.ListenAndServe(":"+s.AppPort, r); err != nil {
			log.Fatalf("Error creating kube2iam http server: %+v", err)
		}
	}

	return nil
}

// NewServer will create a new Server with default values.
func NewServer() *Server {
	return &Server{
		AppPort:                    defaultAppPort,
		MetricsPort:                defaultAppPort,
		BackoffMaxElapsedTime:      defaultMaxElapsedTime,
		IAMRoleKey:                 defaultIAMRoleKey,
		IAMExternalID:              defaultIAMExternalID,
		BackoffMaxInterval:         defaultMaxInterval,
		LogLevel:                   defaultLogLevel,
		LogFormat:                  defaultLogFormat,
		MetadataAddress:            defaultMetadataAddress,
		NamespaceKey:               defaultNamespaceKey,
		CacheResyncPeriod:          defaultCacheResyncPeriod,
		ResolveDupIPs:              defaultResolveDupIPs,
		NamespaceRestrictionFormat: defaultNamespaceRestrictionFormat,
		HealthcheckFailReason:      "",
		IAMRoleSessionTTL:          defaultIAMRoleSessionTTL,
		StsVpcEndPoint:             defaultStsVpcEndpoint,
		BootAsWebServer:            false,
		BootAsWatcher:              false,
	}
}
