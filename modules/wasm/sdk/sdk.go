package sdk

import "github.com/JustinKnueppel/go-result"

var SdkModule = map[string]func(string) result.Result[string]{
	"db.getObject": func(s string) result.Result[string] {
		return result.Ok("test")
	},
}
