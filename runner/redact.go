package runner

import "regexp"

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b(ANTHROPIC_API_KEY|OPENAI_API_KEY|GITHUB_TOKEN|GH_TOKEN)=\S+`), `$1=[REDACTED]`},
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]+`), `[REDACTED]`},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]+`), `[REDACTED]`},
}

func redactSecrets(value string) string {
	for _, pattern := range secretPatterns {
		value = pattern.re.ReplaceAllString(value, pattern.repl)
	}
	return value
}
