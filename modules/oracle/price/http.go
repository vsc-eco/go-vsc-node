package price

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
)

var httpClient *http.Client

func init() {
	jar, _ := cookiejar.New(nil)
	httpClient = &http.Client{
		Jar: jar,
	}
}

func makeUrl(
	baseUrl string,
	queryParams map[string]string,
) (*url.URL, error) {
	url, err := url.Parse(baseUrl)
	if err != nil {
		return nil, err
	}

	if queryParams != nil {
		q := url.Query()
		for key, val := range queryParams {
			q.Add(key, val)
		}

		url.RawQuery = q.Encode()
	}

	return url, nil
}
