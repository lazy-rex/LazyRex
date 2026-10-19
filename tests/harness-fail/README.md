# Harness failure fixtures

Each file here must make `lazyrex-hub test` fail, for the reason in its first line.
`hub/cmd/hub/harness_lab_test.go` runs each one and checks the reason. Do not run this
directory as a normal test suite: every file fails on purpose.
