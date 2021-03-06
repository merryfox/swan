package apiserver

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/src/manager/apiserver/metrics"

	"github.com/Sirupsen/logrus"
	"github.com/emicklei/go-restful"
	"github.com/emicklei/go-restful/swagger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	API_VERSION = "v_beta"
)

type ApiRegister interface {
	Register(*restful.Container)
}

type ApiServer struct {
	addr         string
	sock         string
	apiRegisters []ApiRegister
}

func init() {
	metrics.Register()
}

func NewApiServer(addr, sock string) *ApiServer {
	return &ApiServer{
		addr: addr,
		sock: sock,
	}
}

func Install(apiServer *ApiServer, apiRegister ApiRegister) {
	apiServer.apiRegisters = append(apiServer.apiRegisters, apiRegister)
}

func (apiServer *ApiServer) Start() error {
	wsContainer := restful.NewContainer()

	// Register webservices here
	for _, ws := range apiServer.apiRegisters {
		ws.Register(wsContainer)
	}

	// Add container filter to enable CORS
	cors := restful.CrossOriginResourceSharing{
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH"},
		CookiesAllowed: false,
		Container:      wsContainer}
	wsContainer.Filter(cors.Filter)

	// Add log filter
	wsContainer.Filter(NCSACommonLogFormatLogger())

	// Add prometheus metrics
	wsContainer.Handle("/metrics", promhttp.Handler())

	// Optionally, you can install the Swagger Service which provides a nice Web UI on your REST API
	// You need to download the Swagger HTML5 assets and change the FilePath location in the config below.
	// Open http://localhost:8080/apidocs and enter http://localhost:8080/apidocs.json in the api input field.
	// TODO(xychu): add a config flag for swagger UI, and also for the swagger UI file path.
	swggerUiPath, _ := filepath.Abs("./swagger-ui-2.2.8")
	logrus.Debugf("xychu:  swaggerUIPath: %s", swggerUiPath)
	config := swagger.Config{
		WebServices: wsContainer.RegisteredWebServices(), // you control what services are visible
		// WebServicesUrl: "",
		ApiVersion: API_VERSION,
		ApiPath:    "/apidocs.json",

		// Optionally, specifiy where the UI is located
		SwaggerPath:     "/apidocs/",
		SwaggerFilePath: swggerUiPath,
	}
	swagger.RegisterSwaggerService(config, wsContainer)

	go func() {
		srv := &http.Server{
			Addr:    apiServer.sock,
			Handler: wsContainer,
		}
		ln, err := net.Listen("unix", apiServer.sock)
		if err != nil {
			logrus.Errorf("can't listen on socket %s:%s", apiServer.sock, err.Error())
		}
		logrus.Printf("start listening on %s", apiServer.sock)
		srv.Serve(ln)
	}()

	logrus.Printf("start listening on %s", apiServer.addr)
	server := &http.Server{Addr: apiServer.addr, Handler: wsContainer}
	logrus.Fatal(server.ListenAndServe())

	return nil
}

func NCSACommonLogFormatLogger() restful.FilterFunction {
	return func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		var username = "-"
		if req.Request.URL.User != nil {
			if name := req.Request.URL.User.Username(); name != "" {
				username = name
			}
		}
		chain.ProcessFilter(req, resp)
		logrus.Printf("%s - %s [%s] \"%s %s %s\" %d %d",
			strings.Split(req.Request.RemoteAddr, ":")[0],
			username,
			time.Now().Format("02/Jan/2006:15:04:05 -0700"),
			req.Request.Method,
			req.Request.URL.RequestURI(),
			req.Request.Proto,
			resp.StatusCode(),
			resp.ContentLength(),
		)
	}
}
