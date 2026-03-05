package cloud

import "fmt"

// GenerateRcloneConfig produces an rclone config file for R2 access.
func GenerateRcloneConfig(cfg R2Config) string {
	return fmt.Sprintf(`[r2]
type = s3
provider = Cloudflare
access_key_id = %s
secret_access_key = %s
endpoint = https://%s.r2.cloudflarestorage.com
`, cfg.AccessKeyID, cfg.SecretAccessKey, cfg.AccountID)
}
