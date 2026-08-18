package metrics

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTeamRoutingPrometheusAlerts(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/opentelemetry/prometheus-rules/team-routing-alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse Team routing alert rules: %v", err)
	}
	want := map[string]string{
		"AIAgentTeamRoutingNotReady":         `event="readiness_failure"`,
		"AIAgentTeamConfigReloadRejected":    `event="reload_rejected"`,
		"AIAgentTeamConfigDriftBlockedTask":  `outcome="blocked"`,
		"AIAgentTeamSelectionForbiddenSpike": `outcome="forbidden"`,
		"AIAgentDrainingTeamReceivedNewTask": `outcome="draining"`,
		"AIAgentRetiredTeamReceivedNewTask":  `outcome="retired"`,
	}
	seen := make(map[string]bool, len(want))
	for _, group := range document.Groups {
		for _, rule := range group.Rules {
			metricSelector, ok := want[rule.Alert]
			if !ok {
				continue
			}
			if !strings.Contains(rule.Expr, metricSelector) {
				t.Fatalf("alert %s expression %q does not contain %q", rule.Alert, rule.Expr, metricSelector)
			}
			seen[rule.Alert] = true
		}
	}
	for alert := range want {
		if !seen[alert] {
			t.Errorf("missing Team routing alert %s", alert)
		}
	}
}
