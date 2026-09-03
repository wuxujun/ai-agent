package braineval

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = 3

type Scope struct {
	TenantID  string `yaml:"tenant_id" json:"tenant_id"`
	ProjectID string `yaml:"project_id" json:"project_id"`
}

func (s Scope) Key() string { return s.TenantID + "\x00" + s.ProjectID }

type Thresholds struct {
	LiveAnswerAccuracyDelta    float64 `yaml:"live_answer_accuracy_delta" json:"live_answer_accuracy_delta"`
	OfflineEvidenceRecallDelta float64 `yaml:"offline_evidence_recall_delta" json:"offline_evidence_recall_delta"`
	OfflineP95Ratio            float64 `yaml:"offline_p95_ratio" json:"offline_p95_ratio"`
	LiveTotalTokensRatio       float64 `yaml:"live_total_tokens_ratio" json:"live_total_tokens_ratio"`
}

type ProjectFixture struct {
	Scope Scope  `yaml:"scope" json:"scope"`
	Space string `yaml:"space" json:"space"`
	Root  string `yaml:"root" json:"root"`
}

type Case struct {
	Name                 string   `yaml:"name" json:"name"`
	Category             string   `yaml:"category" json:"category"`
	Scope                Scope    `yaml:"scope" json:"scope"`
	Query                string   `yaml:"query" json:"query"`
	ExpectedClaims       []string `yaml:"expected_claims" json:"expected_claims"`
	ExpectedEvidenceURIs []string `yaml:"expected_evidence_uris" json:"expected_evidence_uris"`
	ForbiddenClaims      []string `yaml:"forbidden_claims" json:"forbidden_claims"`
	ExpectNoAnswer       bool     `yaml:"expect_no_answer" json:"expect_no_answer"`
	Critical             bool     `yaml:"critical" json:"critical"`
}

type Dataset struct {
	Version    int              `yaml:"version" json:"version"`
	Thresholds Thresholds       `yaml:"thresholds" json:"thresholds"`
	Projects   []ProjectFixture `yaml:"projects" json:"projects"`
	Cases      []Case           `yaml:"cases" json:"cases"`
}

func LoadDataset(r io.Reader, baseDir string) (Dataset, error) {
	var dataset Dataset
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&dataset); err != nil {
		return Dataset{}, fmt.Errorf("decode dataset: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Dataset{}, fmt.Errorf("dataset contains more than one YAML document")
		}
		return Dataset{}, fmt.Errorf("decode extra dataset document: %w", err)
	}

	base, err := filepath.Abs(baseDir)
	if err != nil {
		return Dataset{}, fmt.Errorf("resolve dataset base directory: %w", err)
	}
	for i := range dataset.Projects {
		root := strings.TrimSpace(dataset.Projects[i].Root)
		if root == "" {
			continue
		}
		if filepath.IsAbs(root) {
			return Dataset{}, fmt.Errorf("projects[%d].root must be relative", i)
		}
		clean := filepath.Clean(root)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return Dataset{}, fmt.Errorf("projects[%d].root escapes base directory", i)
		}
		dataset.Projects[i].Root = filepath.Join(base, clean)
	}
	if err := dataset.Validate(); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

func (d Dataset) Validate() error {
	if d.Version != SchemaVersion {
		return fmt.Errorf("unsupported dataset version %d", d.Version)
	}
	if d.Thresholds != (Thresholds{0.10, 0.10, 1.50, 1.20}) {
		return fmt.Errorf("thresholds must equal the schema defaults")
	}
	projects := make(map[string]struct{}, len(d.Projects))
	for i, project := range d.Projects {
		if strings.TrimSpace(project.Scope.TenantID) == "" || strings.TrimSpace(project.Scope.ProjectID) == "" {
			return fmt.Errorf("projects[%d].scope must have tenant_id and project_id", i)
		}
		if strings.TrimSpace(project.Space) == "" || strings.TrimSpace(project.Root) == "" {
			return fmt.Errorf("projects[%d] requires non-empty space and root", i)
		}
		if _, exists := projects[project.Scope.Key()]; exists {
			return fmt.Errorf("duplicate project scope %q", project.Scope.Key())
		}
		projects[project.Scope.Key()] = struct{}{}
	}
	cases := make(map[string]struct{}, len(d.Cases))
	for i, c := range d.Cases {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Category) == "" {
			return fmt.Errorf("cases[%d] requires non-empty name and category", i)
		}
		if _, exists := cases[c.Name]; exists {
			return fmt.Errorf("duplicate case name %q", c.Name)
		}
		cases[c.Name] = struct{}{}
		if _, exists := projects[c.Scope.Key()]; !exists {
			return fmt.Errorf("case %q references unknown project scope %q", c.Name, c.Scope.Key())
		}
		if strings.TrimSpace(c.Query) == "" {
			return fmt.Errorf("case %q query must not be empty", c.Name)
		}
		if c.ExpectNoAnswer && len(c.ExpectedClaims) > 0 {
			return fmt.Errorf("case %q cannot expect no answer and expected claims", c.Name)
		}
	}
	return nil
}
