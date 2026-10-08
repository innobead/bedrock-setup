// Command monthly-unpause is the Lambda that runs at 06:00 UTC on the 1st of each month. It saves
// a snapshot of every user's limit and pause state for the month that ended, then re-arms every
// pause action that is paused or off, keeping pauses by hand in place. bedrock-admin apply deploys it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
)

func handler(ctx context.Context) (pause.MonthlyResult, error) {
	bucket, prefix := os.Getenv("SNAPSHOT_BUCKET"), os.Getenv("SNAPSHOT_PREFIX")
	if bucket == "" || prefix == "" {
		return pause.MonthlyResult{}, fmt.Errorf("SNAPSHOT_BUCKET and SNAPSHOT_PREFIX must be set")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return pause.MonthlyResult{}, err
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return pause.MonthlyResult{}, err
	}
	budgetsCfg := cfg.Copy()
	budgetsCfg.Region = "us-east-1"
	m := &pause.Manager{Budgets: budgets.NewFromConfig(budgetsCfg), IAM: iam.NewFromConfig(cfg), Account: aws.ToString(id.Account)}
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.DisableLogOutputChecksumValidationSkipped = true })
	save := func(ctx context.Context, s pause.Snapshot) error {
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		key := pause.SnapshotKey(prefix, s.Month)
		_, err = s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
			Body: bytes.NewReader(b), ContentType: aws.String("application/json")})
		if err == nil {
			log.Printf("saved s3://%s/%s (%d users)", bucket, key, len(s.Users))
		}
		return err
	}
	res, err := m.Monthly(ctx, save)
	for b, d := range res.Done {
		log.Printf("%s: %s", b, d)
	}
	return res, err
}

func main() { lambda.Start(handler) }
