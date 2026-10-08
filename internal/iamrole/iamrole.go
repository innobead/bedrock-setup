// Package iamrole creates, checks and deletes the IAM roles bedrock-admin manages.
package iamrole

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	itypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Spec is the wanted state of a role.
type Spec struct {
	Name        string
	Path        string
	Description string
	Trust       string
	InlineName  string
	Inline      string
	MaxSession  int32 // 0 keeps the AWS default (3600)
	Tags        map[string]string
}

// State is what exists.
type State struct {
	Exists bool
	Arn    string
	Tags   map[string]string
	// Drift lists the differences from the spec, in words.
	Drift []string
	fix   struct{ trust, inline, maxSession bool }
}

// ID is the bedrock-admin id the role was created with ("" if none).
func (s State) ID() string { return s.Tags[policy.IDTag] }

// Observe reads a role and compares it with the spec.
func Observe(ctx context.Context, c *iam.Client, s Spec) (State, error) {
	var st State
	r, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(s.Name)})
	if err != nil {
		if isNoSuchEntity(err) {
			return st, nil
		}
		return st, fmt.Errorf("reading role %s: %w", s.Name, err)
	}
	st.Exists, st.Arn = true, aws.ToString(r.Role.Arn)
	st.Tags = map[string]string{}
	for _, t := range r.Role.Tags {
		st.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if aws.ToString(r.Role.Path) != s.Path {
		st.Drift = append(st.Drift, fmt.Sprintf("path is %s, not %s (needs recreating)", aws.ToString(r.Role.Path), s.Path))
	}
	if !policy.Equal(policy.Decode(aws.ToString(r.Role.AssumeRolePolicyDocument)), s.Trust) {
		st.Drift = append(st.Drift, "trust policy changed outside the file")
		st.fix.trust = true
	}
	if s.MaxSession != 0 && aws.ToInt32(r.Role.MaxSessionDuration) != s.MaxSession {
		st.Drift = append(st.Drift, fmt.Sprintf("max session %ds → %ds", aws.ToInt32(r.Role.MaxSessionDuration), s.MaxSession))
		st.fix.maxSession = true
	}
	if s.InlineName != "" {
		p, err := c.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(s.Name), PolicyName: aws.String(s.InlineName)})
		switch {
		case isNoSuchEntity(err):
			st.Drift = append(st.Drift, "policy "+s.InlineName+" missing")
			st.fix.inline = true
		case err != nil:
			return st, fmt.Errorf("reading policy %s of %s: %w", s.InlineName, s.Name, err)
		case !policy.Equal(policy.Decode(aws.ToString(p.PolicyDocument)), s.Inline):
			st.Drift = append(st.Drift, "policy "+s.InlineName+" changed outside the file")
			st.fix.inline = true
		}
	}
	for _, k := range sortedKeys(s.Tags) {
		if k == policy.IDTag {
			continue
		}
		if have, ok := st.Tags[k]; !ok || have != s.Tags[k] {
			st.Drift = append(st.Drift, fmt.Sprintf("tag %s %q → %q", k, have, s.Tags[k]))
		}
	}
	return st, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func tags(m map[string]string) []itypes.Tag {
	var out []itypes.Tag
	for _, k := range sortedKeys(m) {
		out = append(out, itypes.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// Create creates the role and its inline policy.
func Create(ctx context.Context, c *iam.Client, s Spec) error {
	in := &iam.CreateRoleInput{RoleName: aws.String(s.Name), Path: aws.String(s.Path),
		AssumeRolePolicyDocument: aws.String(s.Trust), Tags: tags(s.Tags)}
	if s.Description != "" {
		in.Description = aws.String(s.Description)
	}
	if s.MaxSession != 0 {
		in.MaxSessionDuration = aws.Int32(s.MaxSession)
	}
	if _, err := c.CreateRole(ctx, in); err != nil {
		return fmt.Errorf("creating role %s: %w", s.Name, err)
	}
	if s.InlineName != "" {
		if _, err := c.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(s.Name),
			PolicyName: aws.String(s.InlineName), PolicyDocument: aws.String(s.Inline)}); err != nil {
			return fmt.Errorf("adding policy %s to %s: %w", s.InlineName, s.Name, err)
		}
	}
	return nil
}

// Fix restores the spec on an existing role.
func Fix(ctx context.Context, c *iam.Client, s Spec, st State) error {
	if st.fix.trust {
		if _, err := c.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(s.Name),
			PolicyDocument: aws.String(s.Trust)}); err != nil {
			return fmt.Errorf("updating trust policy of %s: %w", s.Name, err)
		}
	}
	if st.fix.maxSession {
		if _, err := c.UpdateRole(ctx, &iam.UpdateRoleInput{RoleName: aws.String(s.Name),
			MaxSessionDuration: aws.Int32(s.MaxSession)}); err != nil {
			return fmt.Errorf("updating %s: %w", s.Name, err)
		}
	}
	if st.fix.inline {
		if _, err := c.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(s.Name),
			PolicyName: aws.String(s.InlineName), PolicyDocument: aws.String(s.Inline)}); err != nil {
			return fmt.Errorf("updating policy %s of %s: %w", s.InlineName, s.Name, err)
		}
	}
	var t []itypes.Tag
	for _, k := range sortedKeys(s.Tags) {
		if st.Tags[k] != s.Tags[k] {
			t = append(t, itypes.Tag{Key: aws.String(k), Value: aws.String(s.Tags[k])})
		}
	}
	if len(t) > 0 {
		if _, err := c.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(s.Name), Tags: t}); err != nil {
			return fmt.Errorf("tagging %s: %w", s.Name, err)
		}
	}
	return nil
}

// Delete detaches every managed policy, deletes every inline policy, then the role.
func Delete(ctx context.Context, c *iam.Client, name string) error {
	ap, err := c.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(name)})
	if err != nil {
		if isNoSuchEntity(err) {
			return nil
		}
		return err
	}
	for _, p := range ap.AttachedPolicies {
		if _, err := c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(name), PolicyArn: p.PolicyArn}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("detaching %s from %s: %w", aws.ToString(p.PolicyName), name, err)
		}
	}
	ip, err := c.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(name)})
	if err != nil {
		return err
	}
	for _, p := range ip.PolicyNames {
		if _, err := c.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(p)}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("deleting policy %s of %s: %w", p, name, err)
		}
	}
	if _, err := c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("deleting role %s: %w", name, err)
	}
	return nil
}

// Retry repeats f while IAM changes propagate (a new role can't be used for a few seconds).
func Retry(ctx context.Context, wait time.Duration, retryable func(error) bool, f func() error) error {
	deadline := time.Now().Add(wait)
	for {
		err := f()
		if err == nil || !retryable(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func isNoSuchEntity(err error) bool {
	var e *itypes.NoSuchEntityException
	return errors.As(err, &e)
}
