#!/bin/bash
# Assume the personal role and make one tiny Bedrock call. Prints OK or the error.
ROLE=arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen
read AK SK ST <<< $(aws sts assume-role --role-arn $ROLE --role-session-name achen@example.com \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text)
env -u AWS_PROFILE AWS_ACCESS_KEY_ID=$AK AWS_SECRET_ACCESS_KEY=$SK AWS_SESSION_TOKEN=$ST AWS_REGION=us-west-2 \
  aws bedrock-runtime converse --model-id us.anthropic.claude-haiku-4-5-20251001-v1:0 \
  --messages '[{"role":"user","content":[{"text":"Reply with: OK"}]}]' \
  --query 'output.message.content[0].text' --output text 2>&1 | cut -c1-230
