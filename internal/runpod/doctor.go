package runpod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	toml "github.com/pelletier/go-toml"
)

var ErrTemplateNotFound = errors.New("runpod template not found")

const managedTemplateMarker = "managed-by=weft"

type Manager struct {
	client *CloudClient
}

func NewManager() *Manager {
	return &Manager{client: NewCloudClient()}
}

func newManagerWithClient(client *CloudClient) *Manager {
	return &Manager{client: client}
}

// DesiredBootstrapTemplate returns the desired RunPod bootstrap template spec.
func DesiredBootstrapTemplate(cfg *config.Config) BootstrapTemplateSpec {
	image := cloud.DefaultImage
	if cfg != nil && strings.TrimSpace(cfg.Runpod.DefaultImage) != "" {
		image = strings.TrimSpace(cfg.Runpod.DefaultImage)
	}
	startCommand := cloud.R2BootstrapTemplateStartCmd()
	hashInput := image + "\n" + startCommand
	sum := sha256.Sum256([]byte(hashInput))
	hash := hex.EncodeToString(sum[:8])
	readme := strings.Join([]string{
		"Weft-managed RunPod bootstrap template.",
		managedTemplateMarker,
		"template-kind=bootstrap",
		"spec-hash=" + hash,
	}, "\n")
	return BootstrapTemplateSpec{
		Name:         "weft-bootstrap-" + hash,
		SpecHash:     hash,
		Image:        image,
		StartCommand: startCommand,
		Readme:       readme,
	}
}

// Diagnose inspects local RunPod readiness without mutating config.
func Diagnose(cfg *config.Config) (*Diagnosis, error) {
	return NewManager().Diagnose(cfg)
}

// Setup ensures a compatible bootstrap template exists and persists config.
func Setup(cfg *config.Config) (*SetupResult, error) {
	return NewManager().Setup(cfg)
}

func (m *Manager) Diagnose(cfg *config.Config) (*Diagnosis, error) {
	diag, _, _, err := m.diagnose(cfg)
	return diag, err
}

func (m *Manager) diagnose(cfg *config.Config) (*Diagnosis, BootstrapTemplateSpec, *cliCapabilities, error) {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}

	spec := DesiredBootstrapTemplate(cfg)
	diag := &Diagnosis{
		Enabled:              cfg.Runpod.Enabled,
		TemplateID:           cfg.Runpod.BootstrapTemplateID,
		RequiredStartCommand: spec.StartCommand,
		DefaultImage:         spec.Image,
	}

	ctx := context.Background()
	caps, capErr := m.client.capabilities(ctx)

	searchChecks := []Check{{Name: "runpod.enabled", OK: cfg.Runpod.Enabled, Detail: boolDetail(cfg.Runpod.Enabled, "enabled", "disabled")}}
	if capErr != nil {
		searchChecks = append(searchChecks,
			Check{Name: "runpodctl", OK: false, Detail: capErr.Error()},
			Check{Name: "auth", OK: false, Detail: "skipped because CLI is unavailable or incompatible"},
		)
		diag.SearchChecks = searchChecks
		diag.LaunchChecks = launchChecksForConfig(cfg, diag.TemplateID, nil, false, spec)
		diag.SearchReady = false
		diag.LaunchReady = false
		return diag, spec, nil, nil
	}

	diag.CLIPath = caps.path
	diag.Version = caps.version
	diag.SearchCommand = joinCommand(caps.searchCommand)
	diag.PodCommandFamily = fmt.Sprintf("%s / %s / %s", joinCommand(caps.podListCommand), joinCommand(caps.podGetCommand), joinCommand(caps.podDeleteCommand))
	if len(caps.templateCreateCommand) > 0 {
		diag.TemplateCommandFamily = fmt.Sprintf("%s / %s / %s", joinCommand(caps.templateListCommand), joinCommand(caps.templateGetCommand), joinCommand(caps.templateCreateCommand))
	}

	searchChecks = append(searchChecks, Check{Name: "runpodctl", OK: true, Detail: fmt.Sprintf("%s (%s)", diag.Version, diag.CLIPath)})
	authErr := m.client.checkAuth(ctx, caps)
	searchChecks = append(searchChecks, Check{Name: "auth", OK: authErr == nil, Detail: detailOrOK(authErr, "authenticated")})
	diag.SearchChecks = searchChecks
	diag.SearchReady = cfg.Runpod.Enabled && authErr == nil

	var tmpl *TemplateInfo
	var tmplErr error
	if authErr == nil && diag.TemplateID != "" && len(caps.templateGetCommand) > 0 {
		tmpl, tmplErr = m.client.getTemplate(ctx, caps, diag.TemplateID)
		if tmplErr == nil {
			diag.Template = tmpl
			diag.TemplateCompatible = templateMatchesSpec(tmpl, spec)
		}
	}

	diag.LaunchChecks = launchChecksForConfig(cfg, diag.TemplateID, tmpl, diag.TemplateCompatible, spec)
	if tmplErr != nil {
		diag.LaunchChecks = append(diag.LaunchChecks, Check{Name: "template lookup", OK: false, Detail: tmplErr.Error()})
	}
	diag.LaunchReady = diag.SearchReady && allChecksOK(diag.LaunchChecks)
	return diag, spec, caps, nil
}

func (m *Manager) Setup(cfg *config.Config) (*SetupResult, error) {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}

	diag, spec, caps, err := m.diagnose(cfg)
	if err != nil {
		return nil, err
	}
	result := &SetupResult{Diagnosis: diag, ConfigPath: config.ConfigPath()}
	if !setupPrereqsReady(diag) {
		return result, fmt.Errorf("runpod search is not ready; run `weft runpod doctor` for details")
	}

	ctx := context.Background()

	template := diag.Template
	if template == nil || !diag.TemplateCompatible {
		template, err = m.findReusableTemplate(ctx, caps, spec)
		if err != nil {
			return result, err
		}
		if template == nil {
			template, err = m.client.createTemplate(ctx, caps, spec)
			if err != nil {
				return result, err
			}
			result.CreatedTemplate = true
		}
	}

	if err := config.UpdateGlobalTOML(func(tree *toml.Tree) error {
		tree.SetPath([]string{"runpod", "enabled"}, true)
		tree.SetPath([]string{"runpod", "bootstrap_template_id"}, template.ID)
		return nil
	}); err != nil {
		return result, fmt.Errorf("persist RunPod config: %w", err)
	}
	result.Template = template
	result.UpdatedConfig = true

	updatedCfg := *cfg
	updatedCfg.Runpod.Enabled = true
	updatedCfg.Runpod.BootstrapTemplateID = template.ID
	result.Diagnosis, err = m.Diagnose(&updatedCfg)
	if err != nil {
		return result, err
	}
	return result, nil
}

func setupPrereqsReady(diag *Diagnosis) bool {
	if diag == nil {
		return false
	}
	for _, check := range diag.SearchChecks {
		if check.Name == "runpod.enabled" {
			continue
		}
		if !check.OK {
			return false
		}
	}
	return true
}

func launchChecksForConfig(cfg *config.Config, templateID string, template *TemplateInfo, templateCompatible bool, spec BootstrapTemplateSpec) []Check {
	checks := []Check{
		{Name: "shared R2 config", OK: cfg != nil && cfg.Vastai.R2.Bucket != "" && cfg.Vastai.R2.AccessKeyID != "", Detail: r2Detail(cfg)},
		{Name: "bootstrap_template_id", OK: strings.TrimSpace(templateID) != "", Detail: valueOrMissing(templateID, "missing; run `weft runpod setup`")},
	}
	if templateID == "" {
		return checks
	}
	if template == nil {
		return append(checks, Check{Name: "template compatibility", OK: false, Detail: "template not loaded"})
	}
	detail := "template matches expected image and startup command"
	if !templateCompatible {
		detail = fmt.Sprintf("template %s does not match image=%q and startup command=%q", template.ID, spec.Image, spec.StartCommand)
	}
	return append(checks, Check{Name: "template compatibility", OK: templateCompatible, Detail: detail})
}

func (m *Manager) findReusableTemplate(ctx context.Context, caps *cliCapabilities, spec BootstrapTemplateSpec) (*TemplateInfo, error) {
	templates, err := m.client.listTemplates(ctx, caps)
	if err != nil {
		return nil, err
	}
	for _, tmpl := range templates {
		if tmpl != nil && templateMatchesSpec(tmpl, spec) {
			return tmpl, nil
		}
		if tmpl != nil && strings.Contains(tmpl.Readme, "spec-hash="+spec.SpecHash) {
			return tmpl, nil
		}
	}
	return nil, nil
}

func templateMatchesSpec(tmpl *TemplateInfo, spec BootstrapTemplateSpec) bool {
	if tmpl == nil {
		return false
	}
	if strings.TrimSpace(tmpl.Image) != strings.TrimSpace(spec.Image) {
		return false
	}
	return normalizeStartCommand(tmpl.DockerStartCmd) == normalizeStartCommand(spec.StartCommand)
}

func normalizeStartCommand(cmd string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(cmd)), " ")
}

func allChecksOK(checks []Check) bool {
	for _, check := range checks {
		if !check.OK {
			return false
		}
	}
	return true
}

func boolDetail(ok bool, whenTrue, whenFalse string) string {
	if ok {
		return whenTrue
	}
	return whenFalse
}

func detailOrOK(err error, ok string) string {
	if err == nil {
		return ok
	}
	return err.Error()
}

func valueOrMissing(value, missing string) string {
	if strings.TrimSpace(value) == "" {
		return missing
	}
	return value
}

func r2Detail(cfg *config.Config) string {
	if cfg == nil || cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return "missing `vastai.r2.bucket` or `vastai.r2.access_key_id`"
	}
	return fmt.Sprintf("bucket=%s", cfg.Vastai.R2.Bucket)
}
