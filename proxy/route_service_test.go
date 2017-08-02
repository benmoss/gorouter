package proxy_test

import (
	"bytes"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"code.cloudfoundry.org/gorouter/common/secure"
	"code.cloudfoundry.org/gorouter/routeservice"
	"code.cloudfoundry.org/gorouter/routeservice/header"
	"code.cloudfoundry.org/gorouter/test_util"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

func HaveErrored() types.GomegaMatcher {
	return HaveOccurred()
}

var _ = Describe("Route Services", func() {
	var (
		routeServiceListener net.Listener
		routeServiceURL      string
		routeServiceHandler  func(rw http.ResponseWriter, req *http.Request)
		signatureHeader      string
		metadataHeader       string
		cryptoKey            = "ABCDEFGHIJKLMNOP"
		forwardedUrl         string
	)

	JustBeforeEach(func() {
		server := &http.Server{Handler: http.HandlerFunc(routeServiceHandler)}
		go func() {
			_ = server.Serve(routeServiceListener)
		}()
	})

	BeforeEach(func() {
		conf.RouteServiceEnabled = true
		recommendHttps = true
		forwardedUrl = "https://my_host.com/resource+9-9_9?query=123&query$2=345#page1..5"

		routeServiceHandler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Host).ToNot(Equal("my_host.com"))
			metaHeader := r.Header.Get(routeservice.RouteServiceMetadata)
			sigHeader := r.Header.Get(routeservice.RouteServiceSignature)

			crypto, err := secure.NewAesGCM([]byte(cryptoKey))
			Expect(err).ToNot(HaveOccurred())
			_, err = header.SignatureFromHeaders(sigHeader, metaHeader, crypto)

			Expect(err).ToNot(HaveOccurred())
			Expect(r.Header.Get("X-CF-ApplicationID")).To(Equal(""))

			// validate client request header
			Expect(r.Header.Get("X-CF-Forwarded-Url")).To(Equal(forwardedUrl))

			_, err = w.Write([]byte("My Special Snowflake Route Service\n"))
			Expect(err).ToNot(HaveOccurred())
		}

		crypto, err := secure.NewAesGCM([]byte(cryptoKey))
		Expect(err).ToNot(HaveOccurred())

		config := routeservice.NewRouteServiceConfig(
			testLogger,
			conf.RouteServiceEnabled,
			1*time.Hour,
			crypto,
			nil,
			recommendHttps,
		)
		reqArgs, err := config.Request("", forwardedUrl)
		Expect(err).ToNot(HaveOccurred())
		signatureHeader, metadataHeader = reqArgs.Signature, reqArgs.Metadata

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		routeServiceListener = newTlsListener(ln)
		routeServiceURL = "https://" + routeServiceListener.Addr().String()
	})

	AfterEach(func() {
		err := routeServiceListener.Close()
		Expect(err).ToNot(HaveOccurred())
	})

	Context("with Route Services disabled", func() {
		BeforeEach(func() {
			conf.RouteServiceEnabled = false
			conf.SkipSSLValidation = true
			routeServiceHandler = func(http.ResponseWriter, *http.Request) {
				defer GinkgoRecover()
				Fail("Should not get here into Route Service")
			}
		})

		It("return 502 Bad Gateway", func() {
			ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
				defer GinkgoRecover()
				Fail("Should not get here into the app")
			})
			defer func() {
				Expect(ln.Close()).ToNot(HaveErrored())
			}()

			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "my_host.com", "/", nil)

			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(body).To(ContainSubstring("Support for route services is disabled."))
		})
	})

	Context("with SkipSSLValidation enabled", func() {
		BeforeEach(func() {
			conf.SkipSSLValidation = true
		})

		Context("when a request does not have a valid Route service signature header", func() {
			It("redirects the request to the route service url", func() {
				ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
					defer GinkgoRecover()
					Fail("Should not get here")
				})
				defer func() {
					Expect(ln.Close()).ToNot(HaveErrored())
				}()

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)

				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(res.StatusCode).To(Equal(http.StatusOK))
				Expect(body).To(ContainSubstring("My Special Snowflake Route Service"))
			})

			Context("when the route service is not available", func() {
				It("returns a 502 bad gateway error", func() {
					ln := registerHandlerWithRouteService(r, "my_host.com", "https://bad-route-service", func(conn *test_util.HttpConn) {
						defer GinkgoRecover()
						Fail("Should not get here")
					})
					defer func() {
						Expect(ln.Close()).ToNot(HaveErrored())
					}()

					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)

					conn.WriteRequest(req)

					res, _ := conn.ReadResponse()
					Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
				})
			})
		})

		Context("when a request has a valid Route service signature header", func() {
			BeforeEach(func() {
				routeServiceHandler = func(http.ResponseWriter, *http.Request) {
					defer GinkgoRecover()
					Fail("Should not get here into Route Service")
				}
			})

			It("routes to the backend instance and strips headers", func() {
				ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
					req, _ := conn.ReadRequest()
					Expect(req.Header.Get(routeservice.RouteServiceSignature)).To(Equal(""))
					Expect(req.Header.Get(routeservice.RouteServiceMetadata)).To(Equal(""))
					Expect(req.Header.Get(routeservice.RouteServiceForwardedURL)).To(Equal(""))

					out := &bytes.Buffer{}
					out.WriteString("backend instance")
					res := &http.Response{
						StatusCode: http.StatusOK,
						Body:       ioutil.NopCloser(out),
					}
					conn.WriteResponse(res)
				})
				defer func() {
					Expect(ln.Close()).ToNot(HaveErrored())
				}()

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
				req.Header.Set(routeservice.RouteServiceSignature, signatureHeader)
				req.Header.Set(routeservice.RouteServiceMetadata, metadataHeader)
				req.Header.Set(routeservice.RouteServiceForwardedURL, "http://some-backend-url")
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(body).To(ContainSubstring("backend instance"))
				Expect(res.StatusCode).To(Equal(http.StatusOK))
			})

			Context("when request has Host header with a port", func() {
				It("routes to backend instance and disregards port in Host header", func() {
					ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
						conn.ReadRequest()
						out := &bytes.Buffer{}
						out.WriteString("backend instance")
						res := &http.Response{
							StatusCode: http.StatusOK,
							Body:       ioutil.NopCloser(out),
						}
						conn.WriteResponse(res)
					})
					defer func() {
						Expect(ln.Close()).ToNot(HaveErrored())
					}()

					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
					req.Host = "my_host.com:4444"
					req.Header.Set(routeservice.RouteServiceSignature, signatureHeader)
					req.Header.Set(routeservice.RouteServiceMetadata, metadataHeader)
					req.Header.Set(routeservice.RouteServiceForwardedURL, "http://some-backend-url")
					conn.WriteRequest(req)

					res, body := conn.ReadResponse()
					Expect(body).To(ContainSubstring("backend instance"))
					Expect(res.StatusCode).To(Equal(http.StatusOK))
				})
			})

			Context("and is forwarding to a route service on CF", func() {
				It("does not strip the signature header", func() {
					ln := registerHandler(r, "my_host.com", func(conn *test_util.HttpConn) {
						req, _ := conn.ReadRequest()
						Expect(req.Header.Get(routeservice.RouteServiceSignature)).To(Equal("some-signature"))

						out := &bytes.Buffer{}
						out.WriteString("route service instance")
						res := &http.Response{
							StatusCode: http.StatusOK,
							Body:       ioutil.NopCloser(out),
						}
						conn.WriteResponse(res)
					})
					defer func() {
						Expect(ln.Close()).ToNot(HaveErrored())
					}()

					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
					req.Header.Set(routeservice.RouteServiceSignature, "some-signature")
					conn.WriteRequest(req)

					res, body := conn.ReadResponse()
					Expect(res.StatusCode).To(Equal(http.StatusOK))
					Expect(body).To(ContainSubstring("route service instance"))
				})
			})

			It("returns 502 when backend not available", func() {
				// register route service, should NOT route to it
				registerAddr(r, "my_host.com", routeServiceURL, "localhost:81", "instanceId", "1", "")

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
				req.Header.Set(routeservice.RouteServiceSignature, signatureHeader)
				req.Header.Set(routeservice.RouteServiceMetadata, metadataHeader)
				conn.WriteRequest(req)
				resp, _ := conn.ReadResponse()

				Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			})
		})

		Context("when recommendHttps is set to false", func() {
			BeforeEach(func() {
				recommendHttps = false
				routeServiceHandler = func(w http.ResponseWriter, r *http.Request) {
					Expect(r.Header.Get("X-CF-Forwarded-Url")).To(ContainSubstring("http://"))

					_, err := w.Write([]byte("My Special Snowflake Route Service\n"))
					Expect(err).ToNot(HaveOccurred())
				}
			})

			It("routes to backend over http scheme", func() {
				ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
					defer GinkgoRecover()
					Fail("Should not get here")
				})
				defer func() {
					Expect(ln.Close()).ToNot(HaveErrored())
				}()

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(body).To(ContainSubstring("My Special Snowflake Route Service"))
				Expect(res.StatusCode).To(Equal(http.StatusOK))
			})
		})
	})

	It("returns a 502 when the SSL cert of the route service is signed by an unknown authority", func() {
		ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
			defer GinkgoRecover()
			Fail("Should not get here")
		})
		defer func() {
			Expect(ln.Close()).ToNot(HaveErrored())
		}()

		conn := dialProxy(proxyServer)

		req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
		conn.WriteRequest(req)

		res, _ := readResponse(conn)

		Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
	})

	Context("with a valid certificate", func() {

		It("returns a 200 when we route to a route service", func() {
			ln := registerHandlerWithRouteService(r, "my_host.com", routeServiceURL, func(conn *test_util.HttpConn) {
				defer GinkgoRecover()
				Fail("Should not get here")
			})
			defer func() {
				Expect(ln.Close()).ToNot(HaveErrored())
			}()

			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)
			conn.WriteRequest(req)

			res, _ := readResponse(conn)

			okCodes := []int{http.StatusOK, http.StatusFound}
			Expect(okCodes).Should(ContainElement(res.StatusCode))
		})
	})

	Context("when the route service is a CF app", func() {

		It("successfully looks up the route service and sends the request", func() {

			routeServiceHandler := func(conn *test_util.HttpConn) {
				defer GinkgoRecover()
				resp := test_util.NewResponse(http.StatusOK)
				req, _ := conn.ReadRequest()

				Expect(req.Host).ToNot(Equal("my_app.com"))
				metaHeader := req.Header.Get(routeservice.RouteServiceMetadata)
				sigHeader := req.Header.Get(routeservice.RouteServiceSignature)

				crypto, err := secure.NewAesGCM([]byte(cryptoKey))
				Expect(err).ToNot(HaveOccurred())
				_, err = header.SignatureFromHeaders(sigHeader, metaHeader, crypto)
				Expect(err).ToNot(HaveOccurred())

				// X-CF-ApplicationID will only be set if the request was sent to internal cf app first time
				Expect(req.Header.Get("X-CF-ApplicationID")).To(Equal("my-route-service-app-id"))

				Expect(req.Header.Get("X-CF-Forwarded-Url")).To(Equal("https://my_app.com/"))
				conn.WriteResponse(resp)
			}

			rsListener := registerHandlerWithAppId(r, "route_service.com", "", routeServiceHandler, "", "my-route-service-app-id")
			appListener := registerHandlerWithRouteService(r, "my_app.com", "https://route_service.com", func(conn *test_util.HttpConn) {
				defer GinkgoRecover()
				resp := test_util.NewResponse(http.StatusOK)
				conn.WriteResponse(resp)
				Fail("Should not get here")
			})
			defer func() {
				Expect(rsListener.Close()).ToNot(HaveErrored())
				Expect(appListener.Close()).ToNot(HaveErrored())
			}()
			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "my_app.com", "", nil)
			conn.WriteRequest(req)

			res, _ := readResponse(conn)

			Expect(res.StatusCode).To(Equal(http.StatusOK))
		})
	})
})
