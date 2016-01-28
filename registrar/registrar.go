package registrar

import (
	"os"
	"time"

	"github.com/cloudfoundry-incubator/route-registrar/messagebus"
	"github.com/nu7hatch/gouuid"

	"github.com/cloudfoundry-incubator/route-registrar/config"
	"github.com/cloudfoundry-incubator/route-registrar/healthchecker"

	"github.com/pivotal-golang/lager"
)

type Registrar interface {
	Run(signals <-chan os.Signal, ready chan<- struct{}) error
}

type registrar struct {
	logger            lager.Logger
	config            config.Config
	healthChecker     healthchecker.HealthChecker
	messageBus        messagebus.MessageBus
	privateInstanceId string
}

func NewRegistrar(
	clientConfig config.Config,
	healthChecker healthchecker.HealthChecker,
	logger lager.Logger,
	messageBus messagebus.MessageBus,
) Registrar {
	aUUID, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	return &registrar{
		config:            clientConfig,
		logger:            logger,
		privateInstanceId: aUUID.String(),
		healthChecker:     healthChecker,
		messageBus:        messageBus,
	}
}

type routeHealth struct {
	route   config.Route
	healthy bool
	err     error
}

func (r *registrar) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	var err error

	r.logger.Info("creating nats connection", lager.Data{"config": r.config})

	err = r.messageBus.Connect(r.config.MessageBusServers)
	if err != nil {
		return err
	}
	defer r.messageBus.Close()

	close(ready)

	routeHealthChan := make(chan routeHealth, len(r.config.Routes))

	duration := time.Duration(r.config.UpdateFrequency) * time.Second
	ticker := time.NewTicker(duration)

	for {
		select {
		case s := <-routeHealthChan:
			if s.err != nil {
				r.logger.Info("healthchecker errored for route", lager.Data{"route": s.route})
				err := r.unregisterRoutes(s.route)
				if err != nil {
					return err
				}
			} else if s.healthy {
				r.logger.Info("healthchecker returned healthy for route", lager.Data{"route": s.route})
				err := r.registerRoutes(s.route)
				if err != nil {
					return err
				}
			} else {
				r.logger.Info("healthchecker returned unhealthy for route", lager.Data{"route": s.route})
				err := r.unregisterRoutes(s.route)
				if err != nil {
					return err
				}
			}
		case <-ticker.C:
			for _, route := range r.config.Routes {
				if route.HealthCheck == nil || route.HealthCheck.ScriptPath == "" {
					r.logger.Info("no healthchecker found for route", lager.Data{"route": route})

					err := r.registerRoutes(route)
					if err != nil {
						return err
					}
				} else {
					go func(route config.Route) {
						ok, err := r.healthChecker.Check(route.HealthCheck.ScriptPath, route.HealthCheck.Timeout)
						routeStatus := routeHealth{
							route:   route,
							healthy: ok,
							err:     err,
						}
						routeHealthChan <- routeStatus
					}(route)
				}
			}
		case <-signals:
			r.logger.Info("Received signal; shutting down")

			for _, route := range r.config.Routes {
				err := r.unregisterRoutes(route)
				if err != nil {
					return err
				}
			}
			return nil
		}
	}
}

func (r registrar) registerRoutes(route config.Route) error {
	r.logger.Info("Registering route", lager.Data{"route": route})

	err := r.messageBus.SendMessage("router.register", r.config.Host, route, r.privateInstanceId)
	if err != nil {
		return err
	}

	r.logger.Info("Registered routes successfully")

	return nil
}

func (r registrar) unregisterRoutes(route config.Route) error {
	r.logger.Info("Unregistering route", lager.Data{"route": route})

	err := r.messageBus.SendMessage("router.unregister", r.config.Host, route, r.privateInstanceId)
	if err != nil {
		return err
	}

	r.logger.Info("Unregistered routes successfully")

	return nil
}
