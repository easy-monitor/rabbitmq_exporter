package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

const (
	defaultLogLevel = log.InfoLevel
	serviceName     = "RabbitMQ_exporter"
)

func initLogger() {
	log.SetLevel(getLogLevel())
	if strings.ToUpper(config.OutputFormat) == "JSON" {
		log.SetFormatter(&log.JSONFormatter{})
	} else {
		// The TextFormatter is default, you don't actually have to do this.
		log.SetFormatter(&log.TextFormatter{})
	}
}

func main() {
	var checkURL = flag.String("check-url", "", "Curl url and return exit code (http: 200 => 0, otherwise 1)")
	var configFile = flag.String("config-file", "conf/rabbitmq.conf", "path to json config")
	flag.Parse()

	if *checkURL != "" { // do a single http get request. Used in docker healthckecks as curl is not inside the image
		curl(*checkURL)
		return
	}

	err := initConfigFromFile(*configFile)                  //Try parsing config file
	if _, isPathError := err.(*os.PathError); isPathError { // No file => use environment variables
		initConfig()
	} else if err != nil {
		panic(err)
	}

	initLogger()
	initClient()
	exporter := newExporter()
	prometheus.MustRegister(exporter)

	log.WithFields(log.Fields{
		"VERSION":    Version,
		"REVISION":   Revision,
		"BRANCH":     Branch,
		"BUILD_DATE": BuildDate,
		//		"RABBIT_PASSWORD": config.RABBIT_PASSWORD,
	}).Info("Starting RabbitMQ exporter")

	log.WithFields(log.Fields{
		"PUBLISH_ADDR":        config.PublishAddr,
		"PUBLISH_PORT":        config.PublishPort,
		"RABBIT_URL":          config.RabbitURL,
		"RABBIT_USER":         config.RabbitUsername,
		"OUTPUT_FORMAT":       config.OutputFormat,
		"RABBIT_CAPABILITIES": formatCapabilities(config.RabbitCapabilities),
		"RABBIT_EXPORTERS":    config.EnabledExporters,
		"CAFILE":              config.CAFile,
		"CERTFILE":            config.CertFile,
		"KEYFILE":             config.KeyFile,
		"SKIPVERIFY":          config.InsecureSkipVerify,
		"EXCLUDE_METRICS":     config.ExcludeMetrics,
		"SKIP_QUEUES":         config.SkipQueues.String(),
		"INCLUDE_QUEUES":      config.IncludeQueues,
		"SKIP_VHOST":          config.SkipVHost.String(),
		"INCLUDE_VHOST":       config.IncludeVHost,
		"RABBIT_TIMEOUT":      config.Timeout,
		"MAX_QUEUES":          config.MaxQueues,
		//		"RABBIT_PASSWORD": config.RABBIT_PASSWORD,
	}).Info("Active Configuration")
	conf, err := loadConfig()
	if err != nil {
		panic(err.Error())
	}

	handler := http.NewServeMux()
	handler.Handle("/scrape", handlerForScrape(conf))
	handler.Handle("/metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>RabbitMQ Exporter</title></head>
             <body>
             <h1>RabbitMQ Exporter</h1>
             <p><a href='/metrics'>Metrics</a></p>
             </body>
             </html>`))
	})
	handler.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if exporter.LastScrapeOK() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	})

	server := &http.Server{Addr: config.PublishAddr + ":" + config.PublishPort, Handler: handler}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()

	<-runService()
	log.Info("Shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	cancel()
}

func loadConfig() (*Modules, error) {
	path, _ := os.Getwd()
	path = filepath.Join(path, "conf/conf.yml")
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.New("read conf.yml fail:" + path)
	}
	conf := new(Modules)
	err = yaml.Unmarshal(data, conf)
	if err != nil {
		return nil, errors.New("unmarshal conf.yml fail")
	}
	return conf, nil
}

func handlerForScrape(modules *Modules) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupsUrl := config.RabbitURL
		backupsUsername := config.RabbitUsername
		backupsPassword := config.RabbitPassword
		uri := r.URL.Query()
		target := uri.Get("target")
		module := uri.Get("module")
		if target == "" || module == "" {
			_, _ = w.Write([]byte("args error"))
			return
		}
		var username string
		var password string
		for i := 0; i < len(modules.Modules); i++ {
			if modules.Modules[i].Name == module {
				username = modules.Modules[i].Username
				password = modules.Modules[i].Password
				break
			}
		}
		if username == "" || password == "" {
			_, _ = w.Write([]byte("module not found in conf.yml"))
			return
		}
		if !strings.HasPrefix(target, "http://") {
			target = "http://" + target
		}
		fmt.Println(target)

		config.RabbitURL = target
		config.RabbitUsername = username
		config.RabbitPassword = password

		exporter := newExporter()

		registry := prometheus.NewRegistry()
		registry.MustRegister(exporter)

		gatherers := prometheus.Gatherers{}
		gatherers = append(gatherers, prometheus.DefaultGatherer)
		gatherers = append(gatherers, registry)

		// Delegate http serving to Prometheus client library, which will call collector.Collect.
		h := promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		})

		h.ServeHTTP(w, r)
		config.RabbitURL = backupsUrl
		config.RabbitUsername = backupsUsername
		config.RabbitPassword = backupsPassword

	})
}

func getLogLevel() log.Level {
	lvl := strings.ToLower(os.Getenv("LOG_LEVEL"))
	level, err := log.ParseLevel(lvl)
	if err != nil {
		level = defaultLogLevel
	}
	return level
}

func formatCapabilities(caps rabbitCapabilitySet) string {
	var buffer bytes.Buffer
	first := true
	for k := range caps {
		if !first {
			buffer.WriteString(",")
		}
		first = false
		buffer.WriteString(string(k))
	}
	return buffer.String()
}
