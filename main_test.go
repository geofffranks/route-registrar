package main_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/apcera/nats"
	"github.com/cloudfoundry-incubator/route-registrar/config"
	"github.com/cloudfoundry-incubator/route-registrar/messagebus"
	"github.com/fraenkel/candiedyaml"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Main", func() {
	var (
		natsCmd       *exec.Cmd
		testSpyClient *nats.Conn
	)

	BeforeEach(func() {
		natsUsername := "nats"
		natsPassword := "nats"
		natsHost := "127.0.0.1"

		initConfig()
		writeConfig()

		natsCmd = exec.Command(
			"gnatsd",
			"-p", strconv.Itoa(natsPort),
			"--user", natsUsername,
			"--pass", natsPassword,
		)
		err := natsCmd.Start()

		natsAddress := fmt.Sprintf("127.0.0.1:%d", natsPort)

		Eventually(func() error {
			_, err := net.Dial("tcp", natsAddress)
			return err
		}).Should(Succeed())

		servers := []string{
			fmt.Sprintf(
				"nats://%s:%s@%s:%d",
				natsUsername,
				natsPassword,
				natsHost,
				natsPort,
			),
		}

		opts := nats.DefaultOptions
		opts.Servers = servers

		testSpyClient, err = opts.Connect()

		Expect(err).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		natsCmd.Process.Kill()
		natsCmd.Wait()
	})

	It("Writes pid to the provided pidfile", func() {
		command := exec.Command(
			routeRegistrarBinPath,
			fmt.Sprintf("-pidfile=%s", pidFile),
			fmt.Sprintf("-configPath=%s", configFile),
		)
		session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(session.Out).Should(gbytes.Say("Initializing"))
		Eventually(session.Out).Should(gbytes.Say("Writing pid"))
		Eventually(session.Out).Should(gbytes.Say("Running"))

		session.Kill().Wait()
		Eventually(session).Should(gexec.Exit())

		pidFileContents, err := ioutil.ReadFile(pidFile)
		Expect(err).ShouldNot(HaveOccurred())

		Expect(len(pidFileContents)).To(BeNumerically(">", 0))
	})

	It("registers routes via NATS", func() {
		const (
			topic = "router.register"
		)

		registered := make(chan string)
		testSpyClient.Subscribe(topic, func(msg *nats.Msg) {
			registered <- string(msg.Data)
		})

		command := exec.Command(
			routeRegistrarBinPath,
			fmt.Sprintf("-configPath=%s", configFile),
		)
		session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(session.Out).Should(gbytes.Say("Initializing"))
		Eventually(session.Out).Should(gbytes.Say("Running"))
		Eventually(session.Out, 10*time.Second).Should(gbytes.Say("Registering"))

		var receivedMessage string
		Eventually(registered, 10*time.Second).Should(Receive(&receivedMessage))

		expectedRegistryMessage := messagebus.Message{
			URIs: []string{"uri-1", "uri-2"},
			Host: "127.0.0.1",
			Port: 12345,
			Tags: map[string]string{"tag1": "val1", "tag2": "val2"},
		}

		var registryMessage messagebus.Message
		err = json.Unmarshal([]byte(receivedMessage), &registryMessage)
		Expect(err).ShouldNot(HaveOccurred())

		Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
		Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))
		Expect(registryMessage.Tags).To(Equal(expectedRegistryMessage.Tags))

		session.Kill().Wait()
		Eventually(session).Should(gexec.Exit())
	})

	It("Starts correctly and shuts down on SIGINT", func() {
		command := exec.Command(
			routeRegistrarBinPath,
			fmt.Sprintf("-configPath=%s", configFile),
		)
		session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(session.Out).Should(gbytes.Say("Initializing"))
		Eventually(session.Out).Should(gbytes.Say("Running"))
		Eventually(session.Out, 10*time.Second).Should(gbytes.Say("Registering"))

		session.Interrupt().Wait(10 * time.Second)
		Eventually(session.Out).Should(gbytes.Say("Caught signal"))
		Eventually(session.Out).Should(gbytes.Say("Unregistering"))
		Eventually(session).Should(gexec.Exit())
		Expect(session.ExitCode()).To(BeZero())
	})

	It("Starts correctly and shuts down on SIGTERM", func() {
		command := exec.Command(
			routeRegistrarBinPath,
			fmt.Sprintf("-configPath=%s", configFile),
		)
		session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
		Expect(err).ShouldNot(HaveOccurred())

		Eventually(session.Out).Should(gbytes.Say("Initializing"))
		Eventually(session.Out).Should(gbytes.Say("Running"))
		Eventually(session.Out, 10*time.Second).Should(gbytes.Say("Registering"))

		session.Terminate().Wait(10 * time.Second)
		Eventually(session.Out).Should(gbytes.Say("Caught signal"))
		Eventually(session.Out).Should(gbytes.Say("Unregistering"))
		Eventually(session).Should(gexec.Exit())
		Expect(session.ExitCode()).To(BeZero())
	})

	Context("When the config validatation fails", func() {
		BeforeEach(func() {
			rootConfig.Routes[0].RegistrationInterval = nil
			writeConfig()
		})

		It("exits with error", func() {
			command := exec.Command(
				routeRegistrarBinPath,
				fmt.Sprintf("-configPath=%s", configFile),
			)
			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ShouldNot(HaveOccurred())

			Eventually(session.Out).Should(gbytes.Say("Initializing"))
			Eventually(session.Err).Should(gbytes.Say("Update frequency not provided"))

			Eventually(session).Should(gexec.Exit())
			Expect(session.ExitCode()).ToNot(BeZero())
		})
	})
})

func initConfig() {
	natsPort = 42222 + GinkgoParallelNode()

	registrationInterval := 1

	messageBusServers := []config.MessageBusServer{
		config.MessageBusServer{
			Host:     fmt.Sprintf("127.0.0.1:%d", natsPort),
			User:     "nats",
			Password: "nats",
		},
	}

	routes := []config.Route{
		{
			Name:                 "My route",
			Port:                 12345,
			URIs:                 []string{"uri-1", "uri-2"},
			Tags:                 map[string]string{"tag1": "val1", "tag2": "val2"},
			RegistrationInterval: &registrationInterval,
		},
	}

	rootConfig = config.Config{
		MessageBusServers: messageBusServers,
		Host:              "127.0.0.1",
		Routes:            routes,
	}
}

func writeConfig() {
	fileToWrite, err := os.Create(configFile)
	Expect(err).ShouldNot(HaveOccurred())

	encoder := candiedyaml.NewEncoder(fileToWrite)
	err = encoder.Encode(rootConfig)
	Expect(err).ShouldNot(HaveOccurred())
}
