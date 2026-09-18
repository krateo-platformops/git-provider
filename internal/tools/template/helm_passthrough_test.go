package template

import "testing"

func TestHelmChartPassesThroughVerbatim(t *testing.T) {
	// The file that failed on krateo-057: publish-sock-shop-003 / microservices.yaml
	//   "create failed: unable to copy files: template: template:61: function \"toYaml\" not defined"
	body := `resources:
  {{- toYaml .Values.resources | nindent 4 }}
{{- include "chart.labels" . }}
{{/* a helpers.tpl header */}}
value: {{ .Values.name }}
`
	got, err := Template(body).Render(map[string]any{})
	if err != nil {
		t.Fatalf("a Helm chart must commit verbatim, got render error: %v", err)
	}
	if string(got) != body {
		t.Errorf("content was altered.\n--- want ---\n%s\n--- got ---\n%s", body, got)
	}
}
