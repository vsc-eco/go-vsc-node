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

func makeRequest(
	method string, url *url.URL,
	header map[string]string,
) (*http.Response, error) {
	req, err := http.NewRequest(method, url.String(), nil)
	if err != nil {
		return nil, err
	}

	for k, v := range header {
		req.Header.Add(k, v)
	}

	return httpClient.Do(req)
}
