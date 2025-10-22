package price

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

func httpRequest[T any](
	request *http.Request,
) (*T, error) {
	res, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		buf := bytes.Buffer{}
		if _, err := io.Copy(&buf, res.Body); err != nil {
			return nil, fmt.Errorf("failed to decode error message: %w", err)
		}

		return nil, fmt.Errorf(
			"request failed: [status:%s] [msg:%s]",
			res.Status, buf.String(),
		)
	}

	buf := new(T)
	if err := json.NewDecoder(res.Body).Decode(buf); err != nil {
		return nil, err
	}

	return buf, nil
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
) (*http.Request, error) {
	req, err := http.NewRequest(method, url.String(), nil)
	if err != nil {
		return nil, err
	}

	for k, v := range header {
		req.Header.Add(k, v)
	}

	return req, nil
}
