package integration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"code.cloudfoundry.org/gorouter/config"
	"code.cloudfoundry.org/gorouter/mbus"
	"code.cloudfoundry.org/gorouter/route"
	"code.cloudfoundry.org/gorouter/test_util"

	nats "github.com/nats-io/go-nats"
	yaml "gopkg.in/yaml.v2"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
	. "github.com/onsi/gomega/gexec"
)

type testState struct {
	// these get set by the constructor
	cfg                            *config.Config
	client                         *http.Client
	trustedExternalServiceHostname string
	trustedExternalServiceTLS      *tls.Config

	trustedBackendServerCertSAN string
	trustedBackendTLSConfig     *tls.Config

	trustedClientTLSConfig *tls.Config

	// these get set when gorouter is started
	tmpdir          string
	natsRunner      *test_util.NATSRunner
	gorouterSession *Session
	mbusClient      *nats.Conn
}

func NewTestState() *testState {
	// TODO: don't hide so much behind these test_util methods
	cfg, clientTLSConfig := test_util.SpecSSLConfig(test_util.NextAvailPort(), test_util.NextAvailPort(), test_util.NextAvailPort(), test_util.NextAvailPort())
	cfg.SkipSSLValidation = false
	cfg.CipherSuites = []uint16{tls.TLS_RSA_WITH_AES_256_CBC_SHA}

	// TODO: why these magic numbers?
	cfg.PruneStaleDropletsInterval = 2 * time.Second
	cfg.DropletStaleThreshold = 10 * time.Second
	cfg.StartResponseDelayInterval = 1 * time.Second
	cfg.EndpointTimeout = 5 * time.Second
	cfg.EndpointDialTimeout = 10 * time.Millisecond
	cfg.DrainTimeout = 200 * time.Millisecond
	cfg.DrainWait = 1 * time.Second

	cfg.Backends.MaxConns = 10
	cfg.LoadBalancerHealthyThreshold = 0

	cfg.SuspendPruningIfNatsUnavailable = true

	cfg.DisableKeepAlives = false

	externalRouteServiceHostname := "external-route-service.localhost.routing.cf-app.com"
	routeServiceKey, routeServiceCert := test_util.CreateKeyPair(externalRouteServiceHostname)
	routeServiceTLSCert, err := tls.X509KeyPair(routeServiceCert, routeServiceKey)
	Expect(err).ToNot(HaveOccurred())
	cfg.CACerts = string(routeServiceCert)

	browserToGoRouterClientCertChain := test_util.CreateSignedCertWithRootCA(test_util.CertNames{})
	cfg.CACerts = cfg.CACerts + string(browserToGoRouterClientCertChain.CACertPEM)

	trustedBackendServerCertSAN := "some-trusted-backend.example.net"
	backendCertChain := test_util.CreateSignedCertWithRootCA(test_util.CertNames{CommonName: trustedBackendServerCertSAN})
	cfg.CACerts = cfg.CACerts + string(backendCertChain.CACertPEM)

	gorouterToBackendsClientCertChain := test_util.CreateSignedCertWithRootCA(test_util.CertNames{CommonName: "gorouter"})
	trustedBackendTLSConfig := backendCertChain.AsTLSConfig()
	trustedBackendTLSConfig.ClientAuth = tls.RequireAndVerifyClientCert

	// set Gorouter to use client certs
	cfg.Backends.TLSPem = config.TLSPem{
		CertChain:  string(gorouterToBackendsClientCertChain.CertPEM),
		PrivateKey: string(gorouterToBackendsClientCertChain.PrivKeyPEM),
	}

	// make backend trust the CA that signed the gorouter's client cert
	certPool := x509.NewCertPool()
	certPool.AddCert(gorouterToBackendsClientCertChain.CACert)
	trustedBackendTLSConfig.ClientCAs = certPool

	uaaCACertsPath, err := filepath.Abs(filepath.Join("test", "assets", "certs", "uaa-ca.pem"))
	Expect(err).ToNot(HaveOccurred())

	cfg.OAuth = config.OAuthConfig{
		ClientName:   "client-id",
		ClientSecret: "client-secret",
		CACerts:      uaaCACertsPath,
	}
	cfg.OAuth.TokenEndpoint, cfg.OAuth.Port = hostnameAndPort(oauthServer.Addr())

	return &testState{
		cfg: cfg,
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: clientTLSConfig,
			},
		},
		trustedExternalServiceHostname: externalRouteServiceHostname,
		trustedExternalServiceTLS: &tls.Config{
			Certificates: []tls.Certificate{routeServiceTLSCert},
		},
		trustedClientTLSConfig:      browserToGoRouterClientCertChain.AsTLSConfig(),
		trustedBackendTLSConfig:     trustedBackendTLSConfig,
		trustedBackendServerCertSAN: trustedBackendServerCertSAN,
	}
}

func (s *testState) newRequest(url string) *http.Request {
	req, err := http.NewRequest("GET", url, nil)
	Expect(err).NotTo(HaveOccurred())
	port := s.cfg.Port
	if strings.HasPrefix(url, "https") {
		port = s.cfg.SSLPort
	}
	req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
	return req
}

func (s *testState) register(backend *httptest.Server, routeURI string) {
	s.registerAsTLS(backend, routeURI, "")
}

func (s *testState) registerAsTLS(backend *httptest.Server, routeURI string, serverCertDomainSAN string) {
	_, backendPort := hostnameAndPort(backend.Listener.Addr().String())
	var openPort, tlsPort uint16
	if serverCertDomainSAN != "" {
		tlsPort = uint16(backendPort)
	} else {
		openPort = uint16(backendPort)
	}
	rm := mbus.RegistryMessage{
		Host:                    "127.0.0.1",
		Port:                    openPort,
		TLSPort:                 tlsPort,
		Uris:                    []route.Uri{route.Uri(routeURI)},
		StaleThresholdInSeconds: 1,
		RouteServiceURL:         "",
		PrivateInstanceID:       fmt.Sprintf("%x", rand.Int31()),
		ServerCertDomainSAN:     serverCertDomainSAN,
	}
	s.registerAndWait(rm)
}

func (s *testState) registerWithExternalRouteService(appBackend, routeServiceServer *httptest.Server, routeServiceHostname string, routeURI string) {
	_, serverPort := hostnameAndPort(routeServiceServer.Listener.Addr().String())
	_, appBackendPort := hostnameAndPort(appBackend.Listener.Addr().String())
	rm := mbus.RegistryMessage{
		Host:                    "127.0.0.1",
		Port:                    uint16(appBackendPort),
		Uris:                    []route.Uri{route.Uri(routeURI)},
		StaleThresholdInSeconds: 1,
		RouteServiceURL:         fmt.Sprintf("https://%s:%d", routeServiceHostname, serverPort),
		PrivateInstanceID:       fmt.Sprintf("%x", rand.Int31()),
	}
	s.registerAndWait(rm)
}

func (s *testState) registerWithInternalRouteService(appBackend, routeServiceServer *httptest.Server, routeURI string) {
	_, serverPort := hostnameAndPort(routeServiceServer.Listener.Addr().String())
	internalRouteServiceHostname := fmt.Sprintf("some-internal-route-service-%d.some.domain", serverPort)
	s.register(routeServiceServer, internalRouteServiceHostname)                                               // the route service is just an app registered normally
	s.registerWithExternalRouteService(appBackend, routeServiceServer, internalRouteServiceHostname, routeURI) // register
}

func (s *testState) registerAndWait(rm mbus.RegistryMessage) {
	b, _ := json.Marshal(rm)
	s.mbusClient.Publish("router.register", b)

	routesUri := fmt.Sprintf("http://%s:%s@127.0.0.1:%d/routes", s.cfg.Status.User, s.cfg.Status.Pass, s.cfg.Status.Port)
	Eventually(func() (bool, error) {
		return routeExists(routesUri, string(rm.Uris[0]))
	}).Should(BeTrue())
}

func (s *testState) StartGorouter() {
	Expect(s.cfg).NotTo(BeNil(), "set up test cfg before calling this function")

	s.natsRunner = test_util.NewNATSRunner(int(s.cfg.Nats[0].Port))
	s.natsRunner.Start()

	var err error
	s.tmpdir, err = ioutil.TempDir("", "gorouter")
	Expect(err).ToNot(HaveOccurred())

	cfgFile := filepath.Join(s.tmpdir, "config.yml")

	cfgBytes, err := yaml.Marshal(s.cfg)
	Expect(err).ToNot(HaveOccurred())
	Expect(ioutil.WriteFile(cfgFile, cfgBytes, 0644)).To(Succeed())

	cmd := exec.Command(gorouterPath, "-c", cfgFile)
	s.gorouterSession, err = Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() *Session {
		if s.gorouterSession.ExitCode() >= 0 {
			Fail("gorouter quit early!")
		}
		return s.gorouterSession
	}, 20*time.Second).Should(Say("starting"))
	Eventually(s.gorouterSession, 5*time.Second).Should(Say(`Successfully-connected-to-nats.*localhost:\d+`))
	Eventually(s.gorouterSession, 5*time.Second).Should(Say(`gorouter.started`))

	s.mbusClient, err = newMessageBus(s.cfg)
	Expect(err).ToNot(HaveOccurred())
}

func (s *testState) StopAndCleanup() {
	if s.natsRunner != nil {
		s.natsRunner.Stop()
	}

	os.RemoveAll(s.tmpdir)

	if s.gorouterSession != nil && s.gorouterSession.ExitCode() == -1 {
		Eventually(s.gorouterSession.Terminate(), 5).Should(Exit(0))
	}
}

func assertRequestSucceeds(client *http.Client, req *http.Request) {
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(200))
	_, err = ioutil.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	resp.Body.Close()
}
