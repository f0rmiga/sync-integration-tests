package sync_integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/copilot/api"
	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/cloudfoundry-incubator/cf-test-helpers/generator"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gexec"
)

var _ = Describe("Syncing", func() {
	Describe("Reconciling state between cc and diego", func() {
		Describe("LRP Syncing", func() {
			const BbsAppsDomain = "cf-apps"

			AfterEach(func() {
				domains, err := bbsClient.Domains(logger)
				Expect(err).To(BeNil())
				Expect(domains).To(ContainElement(BbsAppsDomain), "Freshness bump failed!")
			})

			It("restarts processes missing from diego", func() {
				appName := generator.PrefixedRandomName("SITS", "APP")
				Expect(cf.Cf("push", appName, "--no-start", "-d", testConfig.GetAppsDomain(), "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack").Wait(Timeout)).To(Exit(0))
				Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

				Eventually(func() string {
					body, _ := CurlAppRoot(appName)
					return body
				}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

				processGuid := GetProcessGuid(appName)
				DeleteProcessGuidFromDiego(processGuid)

				Eventually(func() error {
					_, err := bbsClient.DesiredLRPByProcessGuid(logger, processGuid)
					return err
				}, Timeout).ShouldNot(HaveOccurred())
			})

			It("refreshes stale processes", func() {
				appName := generator.PrefixedRandomName("SITS", "APP")
				Expect(cf.Cf("push", appName, "--no-start", "-d", testConfig.GetAppsDomain(), "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack").Wait(Timeout)).To(Exit(0))
				Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

				Eventually(func() string {
					body, _ := CurlAppRoot(appName)
					return body
				}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

				desiredLRPs, err := bbsClient.DesiredLRPs(logger, models.DesiredLRPFilter{})
				Expect(err).NotTo(HaveOccurred())

				guid := cf.Cf("app", appName, "--guid").Wait(Timeout).Out.Contents()
				appGuid := strings.TrimSpace(string(guid))

				processGuid := ""

				for _, desiredLRP := range desiredLRPs {
					if strings.Contains(desiredLRP.ProcessGuid, appGuid) {
						processGuid = desiredLRP.ProcessGuid
						break
					}
				}

				Expect(processGuid).NotTo(BeEmpty())

				instances := int32(2)
				bogusAnnotation := "bogus"
				desiredLRPUpdate := models.DesiredLRPUpdate{
					Instances:  &instances,
					Annotation: &bogusAnnotation,
				}
				Expect(bbsClient.UpdateDesiredLRP(logger, processGuid, &desiredLRPUpdate)).To(Succeed())

				Eventually(func() int32 {
					desiredLRP, err := bbsClient.DesiredLRPByProcessGuid(logger, processGuid)
					Expect(err).NotTo(HaveOccurred())
					return desiredLRP.Instances
				}, Timeout).Should(Equal(int32(1)))
			})

			It("cancels processes that should not be running according to CC", func() {
				appName := generator.PrefixedRandomName("SITS", "APP")
				Expect(cf.Cf("push", appName, "--no-start", "-d", testConfig.GetAppsDomain(), "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack").Wait(Timeout)).To(Exit(0))
				Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

				Eventually(func() string {
					body, _ := CurlAppRoot(appName)
					return body
				}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

				desiredLRPs, err := bbsClient.DesiredLRPs(logger, models.DesiredLRPFilter{})
				Expect(err).NotTo(HaveOccurred())

				guid := cf.Cf("app", appName, "--guid").Wait(Timeout).Out.Contents()
				appGuid := strings.TrimSpace(string(guid))

				var desiredLRP models.DesiredLRP

				for _, lrp := range desiredLRPs {
					if strings.Contains(lrp.ProcessGuid, appGuid) {
						desiredLRP = *lrp
						break
					}
				}

				Expect(desiredLRP).NotTo(BeNil())

				Expect(cf.Cf("delete", "-f", appName).Wait(Timeout)).To(Exit(0))

				Eventually(func() int {
					_, statusCode := CurlAppRoot(appName)
					return statusCode
				}, Timeout, "1s").Should(Equal(http.StatusNotFound))

				Expect(bbsClient.DesireLRP(logger, &desiredLRP)).To(Succeed())

				Eventually(func() error {
					_, err := bbsClient.DesiredLRPByProcessGuid(logger, desiredLRP.ProcessGuid)
					return err
				}, Timeout).Should(Equal(models.ErrResourceNotFound))
			})

			Describe("revisions", func() {
				BeforeEach(func() {
					if !testConfig.RunRevisionsTests {
						Skip("skipping revisions tests")
					}
				})

				It("prefers latest_revision to current app state when restarting missing processes", func() {
					appName := generator.PrefixedRandomName("SITS", "APP")
					By("staging OG dora to get a droplet we can set later")
					Expect(cf.Cf("push", appName,
						"-p", "fixtures/dora",
						"-b", "ruby_buildpack",
					).Wait(PushTimeout)).To(Exit(0))
					appGuid := GetAppGuid(appName)
					ogDoraGuid := GetDropletGuidForApp(appGuid)

					Expect(cf.Cf("set-env", appName, "FOO", "og_bar").Wait(ShortTimeout)).To(Exit(0))
					revisionsEnablePath := fmt.Sprintf("/v3/apps/%s/features/revisions", appGuid)
					Expect(cf.Cf("curl", revisionsEnablePath, "-X", "PATCH", "-d", `{"enabled": true}`).Wait(ShortTimeout)).To(Exit(0))

					Expect(cf.Cf("v3-restart", appName).Wait(PushTimeout)).To(Exit(0))

					Eventually(func() string {
						body, _ := CurlAppRoot(appName)
						return body
					}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

					Eventually(func() string {
						body, _ := CurlApp(appName, "/env/FOO")
						return body
					}, Timeout).Should(ContainSubstring("og_bar"))

					By("deploying other dora to be the last intentionally started revision")
					webProcessGuid := GetCCProcessGuidsForType(appGuid, "web")[0]
					newCommand := fmt.Sprintf(`{"command": "%s"}`, "TEST_VAR=real bundle exec rackup config.ru -p $PORT")
					cf.Cf("curl", fmt.Sprintf("/v3/processes/%s", webProcessGuid), "-X", "PATCH", "-d", newCommand)
					Expect(cf.Cf("v3-set-env", appName, "FOO", "ng_bar").Wait(ShortTimeout)).To(Exit(0))
					Expect(cf.Cf("v3-push", appName, "-p", "fixtures/other-dora", "-b", "ruby_buildpack").Wait(PushTimeout)).To(Exit(0))

					Eventually(func() string {
						body, _ := CurlAppRoot(appName)
						return body
					}, Timeout).Should(ContainSubstring("Hi, I'm Other Dora!"))

					Eventually(func() string {
						body, _ := CurlApp(appName, "/env/FOO")
						return body
					}, Timeout).Should(ContainSubstring("ng_bar"))

					Eventually(func() string {
						body, _ := CurlApp(appName, "/env/TEST_VAR")
						return body
					}, Timeout).Should(ContainSubstring("real"))

					By("setting droplet back to OG dora and the env var back to og_bar")
					webProcessGuid = GetCCProcessGuidsForType(appGuid, "web")[0]
					newCommand = fmt.Sprintf(`{"command": "%s"}`, "TEST_VAR=fake bundle exec rackup config.ru -p $PORT")
					cf.Cf("curl", fmt.Sprintf("/v3/processes/%s", webProcessGuid), "-X", "PATCH", "-d", newCommand)
					Expect(cf.Cf("v3-set-env", appName, "FOO", "og_bar").Wait(ShortTimeout)).To(Exit(0))
					Expect(cf.Cf("v3-set-droplet", appName, "-d", ogDoraGuid).Wait(ShortTimeout)).To(Exit(0))

					processGuid := GetProcessGuid(appName)
					DeleteProcessGuidFromDiego(processGuid)

					Eventually(func() error {
						_, err := bbsClient.DesiredLRPByProcessGuid(logger, processGuid)
						return err
					}, PushTimeout).ShouldNot(HaveOccurred())

					By("when everything has converged, we should be running the last intentionally started revision")
					Eventually(func() string {
						body, _ := CurlAppRoot(appName)
						return body
					}, Timeout).Should(ContainSubstring("Hi, I'm Other Dora!"))

					Eventually(func() string {
						body, _ := CurlApp(appName, "/env/FOO")
						return body
					}, Timeout).Should(ContainSubstring("ng_bar"))

					Eventually(func() string {
						body, _ := CurlApp(appName, "/env/TEST_VAR")
						return body
					}, Timeout).Should(ContainSubstring("real"))
				})
			})
		})
	})

	Describe("Reconciling state between cc and copilot", func() {
		BeforeEach(func() {
			if !runRouteTests {
				Skip("skipping routing tests")
			}
		})

		Describe("Route syncing", func() {
			It("Adds missing routes to copilot", func() {
				appName := generator.PrefixedRandomName("SITS", "APP")
				Expect(cf.Cf("push", appName, "--no-start", "-d", testConfig.GetAppsDomain(), "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack", "--hostname", appName).Wait(Timeout)).To(Exit(0))
				Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

				Eventually(func() string {
					body, _ := CurlAppRoot(appName)
					return body
				}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

				routeGuid := GetRouteGuid(appName)
				Expect(routeGuid).NotTo(BeEmpty())

				_, err := copilotClient.DeleteRoute(context.Background(), &api.DeleteRouteRequest{
					Guid: routeGuid,
				})
				Expect(err).NotTo(HaveOccurred())

				desiredRoute := fmt.Sprintf("%s.%s", strings.ToLower(appName), testConfig.GetAppsDomain())
				Eventually(func() string {
					response, err := copilotClient.ListCfRoutes(context.Background(), &api.ListCfRoutesRequest{})
					Expect(err).NotTo(HaveOccurred())
					return response.Routes[routeGuid]
				}, Timeout).Should(Equal(desiredRoute))
			})

			It("Removes extraneous routes from copilot", func() {
				extraneousRouteGuid := generator.PrefixedRandomName("SITS", "GUID")
				_, err := copilotClient.UpsertRoute(context.Background(), &api.UpsertRouteRequest{
					Route: &api.Route{
						Guid: extraneousRouteGuid,
						Host: "some-host.example.com",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() string {
					response, err := copilotClient.ListCfRoutes(context.Background(), &api.ListCfRoutesRequest{})
					Expect(err).NotTo(HaveOccurred())
					return response.Routes[extraneousRouteGuid]
				}, Timeout).Should(BeEmpty())
			})
		})

		Describe("RouteMappings syncing", func() {
			It("Adds missing route mappings to copilot", func() {
				appName := generator.PrefixedRandomName("SITS", "APP")

				Expect(cf.Cf("push", appName, "--no-start", "--no-route", "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack").Wait(Timeout)).To(Exit(0))
				Expect(cf.Cf("create-route", testSetup.RegularUserContext().Space, testConfig.GetAppsDomain(), "--hostname", appName).Wait(Timeout)).To(Exit(0))

				appGUID := GetAppGuid(appName)

				getRoutePath := fmt.Sprintf("/v2/routes?q=host:%s", appName)
				routeBody := cf.Cf("curl", getRoutePath).Wait().Out.Contents()

				var routeJSON routeList
				json.Unmarshal([]byte(routeBody), &routeJSON)
				routeGUID := routeJSON.Resources[0].Metadata.Guid

				body := struct {
					Relationship relationship `json:"relationships"`
					Weight       int          `json:"weight"`
				}{
					Relationship: relationship{
						App: map[string]string{
							"guid": appGUID,
						},
						Route: map[string]string{
							"guid": routeGUID,
						},
					},
					Weight: 2,
				}

				bodyJSON, err := json.Marshal(body)
				Expect(err).NotTo(HaveOccurred())

				Expect(cf.Cf("curl", "/v3/route_mappings", "-H", "Content-Type: application/json", "-X", "POST", "-d", fmt.Sprintf(`'%s'`, string(bodyJSON))).Wait(Timeout)).To(Exit(0))

				Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

				Eventually(func() string {
					body, _ := CurlAppRoot(appName)
					return body
				}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

				routeMapping := &api.RouteMapping{
					RouteGuid:       routeGUID,
					CapiProcessGuid: appGUID,
					RouteWeight:     2,
				}
				_, err = copilotClient.UnmapRoute(context.Background(), &api.UnmapRouteRequest{
					RouteMapping: routeMapping,
				})
				Expect(err).NotTo(HaveOccurred())

				var returnedMapping *api.RouteMapping
				Eventually(func() bool {
					var found bool
					response, err := copilotClient.ListCfRouteMappings(context.Background(), &api.ListCfRouteMappingsRequest{})
					Expect(err).NotTo(HaveOccurred())

					if mapping, ok := response.RouteMappings[fmt.Sprintf("%s-%s", routeGUID, appGUID)]; ok {
						returnedMapping = mapping
						found = true
					}

					return found
				}, Timeout).Should(BeTrue())

				Expect(returnedMapping.RouteGuid).To(Equal(routeGUID))
				Expect(returnedMapping.CapiProcessGuid).To(Equal(appGUID))
				Expect(returnedMapping.RouteWeight).To(Equal(int32(2)))
			})

			It("Removes extraneous route mappings from copilot", func() {
				extraneousRouteGuid := generator.PrefixedRandomName("SITS", "GUID")
				extraneousAppGuid := generator.PrefixedRandomName("SITS", "GUID")
				routeMapping := &api.RouteMapping{
					RouteGuid:       extraneousRouteGuid,
					CapiProcessGuid: extraneousAppGuid,
					RouteWeight:     1,
				}
				_, err := copilotClient.MapRoute(context.Background(), &api.MapRouteRequest{
					RouteMapping: routeMapping,
				})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() *api.RouteMapping {
					response, err := copilotClient.ListCfRouteMappings(context.Background(), &api.ListCfRouteMappingsRequest{})
					Expect(err).NotTo(HaveOccurred())
					return response.RouteMappings[fmt.Sprintf("%s-%s", extraneousRouteGuid, extraneousAppGuid)]
				}, Timeout).Should(BeNil())
			})

			Describe("CAPIDiegoProcessAssociation syncing", func() {
				It("Adds missing CAPI Diego Process Associations to copilot", func() {
					appName := generator.PrefixedRandomName("SITS", "APP")
					Expect(cf.Cf("push", appName, "--no-start", "-d", testConfig.GetAppsDomain(), "-s", "cflinuxfs3", "-p", "fixtures/dora", "-b", "ruby_buildpack").Wait(Timeout)).To(Exit(0))
					Expect(cf.Cf("start", appName).Wait(PushTimeout)).To(Exit(0))

					Eventually(func() string {
						body, _ := CurlAppRoot(appName)
						return body
					}, Timeout).Should(ContainSubstring("Hi, I'm Dora!"))

					guid := cf.Cf("app", appName, "--guid").Wait(Timeout).Out.Contents()
					appGuid := strings.TrimSpace(string(guid))
					Expect(appGuid).NotTo(BeEmpty())

					appInfo := cf.Cf("curl", fmt.Sprintf("/v2/apps/%s", appGuid)).Wait(Timeout).Out.Contents()
					Expect(appInfo).NotTo(BeEmpty())

					type appResponse struct {
						Metadata struct {
							Guid string
						}
						Entity struct {
							Version string
						}
					}
					a := &appResponse{}

					json.Unmarshal(appInfo, a)
					capiProcessVersion := a.Entity.Version
					Expect(capiProcessVersion).NotTo(BeEmpty())

					diegoProcessGuids := &api.DiegoProcessGuids{
						DiegoProcessGuids: []string{fmt.Sprintf("%s-%s", appGuid, capiProcessVersion)},
					}

					_, err := copilotClient.DeleteCapiDiegoProcessAssociation(context.Background(), &api.DeleteCapiDiegoProcessAssociationRequest{
						CapiProcessGuid: appGuid,
					})
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() *api.DiegoProcessGuids {
						response, err := copilotClient.ListCapiDiegoProcessAssociations(context.Background(), &api.ListCapiDiegoProcessAssociationsRequest{})
						Expect(err).NotTo(HaveOccurred())
						return response.CapiDiegoProcessAssociations[appGuid]
					}, Timeout).Should(Equal(diegoProcessGuids))
				})

				It("Removes extraneous CAPI Diego Process Associations from copilot", func() {
					extraneousAppVersion := generator.PrefixedRandomName("SITS", "V")
					extraneousAppGuid := generator.PrefixedRandomName("SITS", "GUID")
					capiDiegoProcessAssociation := &api.CapiDiegoProcessAssociation{
						CapiProcessGuid:   extraneousAppGuid,
						DiegoProcessGuids: []string{fmt.Sprintf("%s-%s", extraneousAppGuid, extraneousAppVersion)},
					}
					_, err := copilotClient.UpsertCapiDiegoProcessAssociation(context.Background(), &api.UpsertCapiDiegoProcessAssociationRequest{
						CapiDiegoProcessAssociation: capiDiegoProcessAssociation,
					})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() *api.DiegoProcessGuids {
						response, err := copilotClient.ListCapiDiegoProcessAssociations(context.Background(), &api.ListCapiDiegoProcessAssociationsRequest{})
						Expect(err).NotTo(HaveOccurred())
						return response.CapiDiegoProcessAssociations[extraneousAppGuid]
					}, Timeout).Should(BeNil())
				})
			})
		})
	})
})
