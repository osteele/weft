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
	image := effectiveRunpodImage(cfg)
	startCommand := templateStartCommand(cloud.R2BootstrapTemplateStartCmd())
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

// Setup enables RunPod and persists a compatible default image.
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
	imageCheckOK, imageCheckDetail := runpodImageCheck(cfg)
	diag := &Diagnosis{
		Enabled:              cfg.Runpod.Enabled,
		TemplateID:           cfg.Runpod.BootstrapTemplateID,
		RequiredStartCommand: "",
		DefaultImage:         effectiveRunpodImage(cfg),
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
		diag.LaunchChecks = launchChecksForConfig(cfg, imageCheckOK, imageCheckDetail)
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

	diag.LaunchChecks = launchChecksForConfig(cfg, imageCheckOK, imageCheckDetail)
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

	image := effectiveRunpodImage(cfg)
	templateCfg := *cfg
	templateCfg.Runpod.DefaultImage = image
	spec = DesiredBootstrapTemplate(&templateCfg)

	template, created, err := m.ensureBootstrapTemplate(context.Background(), caps, spec)
	if err != nil {
		return result, fmt.Errorf("ensure RunPod bootstrap template: %w", err)
	}
	result.Template = template
	result.CreatedTemplate = created

	if err := config.UpdateGlobalTOML(func(tree *toml.Tree) error {
		tree.SetPath([]string{"runpod", "enabled"}, true)
		tree.SetPath([]string{"runpod", "default_image"}, image)
		if template != nil && template.ID != "" {
			tree.SetPath([]string{"runpod", "bootstrap_template_id"}, template.ID)
		}
		return nil
	}); err != nil {
		return result, fmt.Errorf("persist RunPod config: %w", err)
	}
	result.UpdatedConfig = true

	updatedCfg := *cfg
	updatedCfg.Runpod.Enabled = true
	updatedCfg.Runpod.DefaultImage = image
	if template != nil {
		updatedCfg.Runpod.BootstrapTemplateID = template.ID
	}
	result.Diagnosis, err = m.Diagnose(&updatedCfg)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (m *Manager) ensureBootstrapTemplate(ctx context.Context, caps *cliCapabilities, spec BootstrapTemplateSpec) (*TemplateInfo, bool, error) {
	if caps == nil || len(caps.templateCreateCommand) == 0 || len(caps.templateGetCommand) == 0 || len(caps.templateListCommand) == 0 {
		return nil, false, fmt.Errorf("runpodctl template commands are unavailable")
	}
	template, err := m.findReusableTemplate(ctx, caps, spec)
	if err != nil {
		return nil, false, err
	}
	if template != nil {
		return template, false, nil
	}
	template, err = m.client.createTemplate(ctx, caps, spec)
	if err != nil {
		return nil, false, err
	}
	return template, true, nil
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

func launchChecksForConfig(cfg *config.Config, imageCheckOK bool, imageCheckDetail string) []Check {
	checks := []Check{
		{Name: "shared R2 config", OK: cfg != nil && cfg.Vastai.R2.Bucket != "" && cfg.Vastai.R2.AccessKeyID != "", Detail: r2Detail(cfg)},
		{Name: "runpod.default_image", OK: imageCheckOK, Detail: imageCheckDetail},
	}
	return checks
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
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "bash,-lc,") {
		cmd = "bash -lc " + strings.TrimPrefix(cmd, "bash,-lc,")
	}
	if strings.HasPrefix(cmd, "bash,-c,") {
		cmd = "bash -c " + strings.TrimPrefix(cmd, "bash,-c,")
	}
	return strings.Join(strings.Fields(cmd), " ")
}

func templateStartCommand(script string) string {
	script = strings.TrimSpace(script)
	if script == "" {
		return ""
	}
	return "bash -lc " + script
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

func isRunpodImage(image string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(image)), "runpod/")
}

func effectiveRunpodImage(cfg *config.Config) string {
	if cfg != nil {
		image := strings.TrimSpace(cfg.Runpod.DefaultImage)
		if isRunpodImage(image) {
			return image
		}
	}
	return cloud.DefaultRunpodImage
}

func runpodImageCheck(cfg *config.Config) (bool, string) {
	if cfg == nil {
		return true, fmt.Sprintf("using default image %q", cloud.DefaultRunpodImage)
	}
	image := strings.TrimSpace(cfg.Runpod.DefaultImage)
	if image == "" {
		return true, fmt.Sprintf("using default image %q", cloud.DefaultRunpodImage)
	}
	if isRunpodImage(image) {
		return true, fmt.Sprintf("configured image %q", image)
	}
	return false, fmt.Sprintf("configured image %q is incompatible; expected runpod/*", image)
}
