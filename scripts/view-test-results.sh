#!/bin/bash
go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest
cat $@ | ~/go/bin/gotestfmt 