package policy

import (
	"net/url"
	"testing"
)

func TestEqualIgnoresFormatting(t *testing.T) {
	a := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["b","a"],"Resource":["x"]}]}`
	b := url.QueryEscape(`{"Statement":{"Resource":"x","Action":["a","b"],"Effect":"Allow"},"Version":"2012-10-17"}`)
	if !Equal(a, b) {
		t.Fatal("want equal")
	}
	if Equal(a, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":["a","b"],"Resource":"x"}]}`) {
		t.Fatal("want different")
	}
}

func TestHasBlock(t *testing.T) {
	if !HasBlock(Block("BedrockAdmin")) {
		t.Fatal("block policy must contain the block")
	}
	if !HasBlock(`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"},{"Effect":"Deny","Action":"bedrock:*","Resource":"*"}]}`) {
		t.Fatal("bedrock:* deny covers it")
	}
	if HasBlock(`{"Statement":{"Effect":"Deny","Action":["bedrock:InvokeModel"],"Resource":"*"}}`) {
		t.Fatal("partial deny is not the block")
	}
}

func TestAllowsAssume(t *testing.T) {
	if !AllowsAssumePersonalRole(AssumePersonalRoles()) {
		t.Fatal("want allowed")
	}
	if AllowsAssumePersonalRole(`{"Statement":{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"arn:aws:iam::1:role/other"}}`) {
		t.Fatal("want not allowed")
	}
}
