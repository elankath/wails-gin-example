package web

import (
	"context"
	"crypto/tls"
	"embed"
	"my-app/backend.new/app"
	"my-app/backend.new/model"
	"my-app/backend.new/utils"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sync/errgroup"
)

//go:embed certs
var certs embed.FS

var (
	instance *web
	once     sync.Once
)

type web struct {
	isRunning bool
	errGroup  errgroup.Group
	http      *http.Server // redirector
	https     *http.Server // server (tls)
}

func Web() *web {
	once.Do(func() {
		app.App().UseEnv(func(env *app.Env) {
			// config gin if application is set to log to file
			if env.IsLog2File() {
				gin.SetMode(gin.ReleaseMode)
				gin.DisableConsoleColor()
			}
		})

		// set gin logger
		gin.DefaultWriter = app.App().Log().Web().Writer()

		instance = &web{
			isRunning: false,
		}
	})
	return instance
}

// IsRunning check if the web server is running
func (w *web) IsRunning() bool {
	return w.isRunning
}

// Start Start the web server to run
func (w *web) Start() (ok bool) {
	if w.isRunning {
		return false
	}

	w.reset()
	w.errGroup.Go(func() error {
		return w.http.ListenAndServe()
	})
	w.errGroup.Go(func() error {
		return w.https.ListenAndServeTLS("", "")
	})

	w.isRunning = true
	return true
}

// Stop stop the web server from running
func (w *web) Stop() (ok bool) {
	if w.isRunning {
		ctxHttp, cancelHttp := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelHttp()
		if err := w.http.Shutdown(ctxHttp); err != nil && err != http.ErrServerClosed {
			app.App().Log().Web().Printf("server (http) shutdown error: %+v\n", err)
		}

		ctxHttps, cancelHttps := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelHttps()
		if err := w.https.Shutdown(ctxHttps); err != nil && err != http.ErrServerClosed {
			app.App().Log().Web().Printf("server (http/s) shutdown error: %+v\n", err)
		}

		if err := w.errGroup.Wait(); err != nil && err != http.ErrServerClosed {
			app.App().Log().Web().Printf("server running error: %+v\n", err)
		}

		w.isRunning = false
		return true
	}
	return false
}

// reset initialze http redirector and https server (tls) of the web server
func (w *web) reset() {
	app.App().UseConfig(func(cfg *app.Config) {
		portHttp := cfg.Get(model.OptionNameWebPortHttp)
		portHttps := cfg.Get(model.OptionNameWebPortHttps)
		dirCerts := cfg.Get(model.OptionNameWebDirCerts)

		manager := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(dirCerts),
		}

		w.http = &http.Server{
			Addr: portHttp,
			Handler: manager.HTTPHandler(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				target := "https://" + strings.Replace(r.Host, portHttp, portHttps, 1) + r.RequestURI
				http.Redirect(rw, r, target, http.StatusMovedPermanently)
			})),
		}

		tlsConfig := manager.TLSConfig()
		tlsConfig.GetCertificate = w.getSelfSignedOrLetsEncryptCert(manager)

		w.https = &http.Server{
			Addr:      portHttps,
			Handler:   w.router(),
			TLSConfig: tlsConfig,
		}
	})
}

// getSelfSignedOrLetsEncryptCert override tlsConfig.GetCertificate
// to enable self-signed certs
func (s *web) getSelfSignedOrLetsEncryptCert(certManager *autocert.Manager) func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	var certificate tls.Certificate
	var err error
	hasExternalCerts := false
	dirCache, ok := certManager.Cache.(autocert.DirCache)
	dirCerts := string(dirCache)
	if ok && utils.Utils().HasDir(string(dirCerts)) { // if external dirCerts is set and occured
		hasExternalCerts = true
	} else { // if dirCerts is empty in config, use embed certs
		assetHelper := utils.NewEmbedFS(certs, "certs")
		if err := assetHelper.Extract(dirCerts); err != nil {
			app.App().Log().Web().Fatalf("failed to extract embed certs into dirCerts (%s): %+v\n", dirCerts, err)
		}

		crt, _ := assetHelper.GetFileBytes("localhost.crt")
		key, _ := assetHelper.GetFileBytes("localhost.key")
		certificate, err = tls.X509KeyPair(crt, key)
	}

	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hasExternalCerts {
			// load certs (based on domain name) from external dirCerts if it is set in config
			keyFile := filepath.Join(dirCerts, hello.ServerName+".key")
			crtFile := filepath.Join(dirCerts, hello.ServerName+".crt")
			certificate, err = tls.LoadX509KeyPair(crtFile, keyFile)
		}
		if err != nil {
			// fallback to default cert
			return certManager.GetCertificate(hello)
		}
		return &certificate, err
	}
}
