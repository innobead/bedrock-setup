package config

import "strings"

// DefaultModels returns the models configure suggests and bedrock doctor checks for a region:
// Claude Opus, Sonnet and Haiku 5.5, through the region's geographic inference profile (us., eu.),
// or the global one elsewhere (apac. doesn't offer them).
func DefaultModels(region string) []string {
	geo := strings.SplitN(region, "-", 2)[0]
	if geo != "us" && geo != "eu" {
		geo = "global"
	}
	var out []string
	for _, m := range []string{"opus", "sonnet", "haiku"} {
		out = append(out, geo+".anthropic.claude-"+m+"-5-5")
	}
	return out
}
