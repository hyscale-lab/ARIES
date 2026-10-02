package harness

import (
	"errors"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/sirupsen/logrus"
)

type closeDeployment struct {
	deployment.Deployment
	closes int
	err    error
}

func (d *closeDeployment) Close() error { d.closes++; return d.err }

func TestHarnessConstructionOwnsDeployment(t *testing.T) {
	constructors := map[string]func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger, deployment.Deployment) (app.HarnessInstance, error){
		"Hermes": NewHermes, "OpenClaw": NewOpenClaw,
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			for _, failure := range []string{"invalid MCP", "invalid image", "success"} {
				t.Run(failure, func(t *testing.T) {
					closeErr := errors.New("transport close failure")
					transport := &closeDeployment{err: closeErr}
					cfg := config.Config{}
					cfg.Versions.Hermes.Image = "test/hermes:v1"
					cfg.Versions.OpenClaw.Image = "test/openclaw:v1"
					switch failure {
					case "invalid MCP":
						cfg.Harness.MCPServers = []core.MCPServerConfig{{Name: "invalid"}}
					case "invalid image":
						cfg.Versions.Hermes.Image = ""
						cfg.Versions.OpenClaw.Image = ""
					}
					instance, err := construct(cfg, t.TempDir(), nil, nil, transport)
					if failure == "success" {
						if err != nil {
							t.Fatal(err)
						}
						if transport.closes != 0 {
							t.Fatal("transport closed before manager ownership ended")
						}
						err = instance.Close()
					} else {
						if instance.Harness != nil || err == nil {
							t.Fatalf("instance=%#v, err=%v", instance, err)
						}
						if failure == "invalid MCP" && !strings.Contains(err.Error(), "invalid mcp server config") {
							t.Fatalf("missing MCP validation error: %v", err)
						}
						if failure == "invalid image" && !strings.Contains(err.Error(), "image") {
							t.Fatalf("missing constructor error: %v", err)
						}
					}
					if !errors.Is(err, closeErr) || transport.closes != 1 {
						t.Fatalf("close count=%d err=%v", transport.closes, err)
					}
				})
			}
		})
	}
}
