`go generate ./internal/lambdacode` writes `bootstrap.zip` here (the monthly Lambda, built from
`cmd/monthly-unpause`). It is not committed; `make`, CI and the release build generate it.
