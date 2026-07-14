package sanitize

import "regexp"

var secretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?is)-----BEGIN [^-\n]*PRIVATE KEY-----.*?-----END [^-\n]*PRIVATE KEY-----`), `[REDACTED PRIVATE KEY]`},
	{regexp.MustCompile(`(?is)-----BEGIN [^-\n]*PRIVATE KEY-----.*`), `[REDACTED PRIVATE KEY]`},
	{regexp.MustCompile(`(?i)(authorization.{0,20}bearer\s+)[A-Za-z0-9._~+/=-]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}`), `[REDACTED API KEY]`},
	{regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`), `[REDACTED AWS KEY]`},
	{regexp.MustCompile(`\bgh[psour]_[A-Za-z0-9]{20,}\b`), `[REDACTED GITHUB TOKEN]`},
	{regexp.MustCompile(`(?m)^([+ -]?\s*["']?[A-Z0-9_]*(?:API_KEY|SECRET|TOKEN|PASSWORD|MASTER_KEY)[A-Z0-9_]*["']?\s*[:=]\s*)\S+.*$`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?im)^([+ -]?\s*["']?(?:api[_-]?key|secret|token|password|authorization)["']?\s*[:=]\s*)\S+.*$`), `${1}[REDACTED]`},
}

func Secrets(value string) string {
	for _, rule := range secretPatterns {
		value = rule.pattern.ReplaceAllString(value, rule.replacement)
	}
	return value
}
