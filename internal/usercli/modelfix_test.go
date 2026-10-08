package usercli

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestModelFix(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException"}
	cases := []struct{ model, want string }{
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", "paused"},
		{"us.anthropic.claude-fable-1-v1:0", "not Fable"},
		{"amazon.nova-pro-v1:0", "only call Anthropic Claude"},
	}
	for _, c := range cases {
		if got := modelFix(denied, "us-west-2", c.model); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want it to contain %q", c.model, got, c.want)
		}
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[string]string{
		"operation error Bedrock Runtime: Converse, https response error StatusCode: 403, RequestID: a35e, AccessDeniedException: ":   "AccessDeniedException",
		"operation error STS: AssumeRole, https response error StatusCode: 403, RequestID: x, api error AccessDenied: not authorized": "AccessDenied: not authorized",
		"plain": "plain",
	} {
		if got := short(errors.New(in)).Error(); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
