#!/bin/bash
go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@v2.5.0
cat $@ | ~/go/bin/gotestfmt 