package ctrld

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"github.com/lucas-clemente/quic-go/http3"
	"github.com/miekg/dns"
)

func newDohResolver(uc *UpstreamConfig) *dohResolver {
	r := &dohResolver{
		endpoint:          uc.Endpoint,
		isDoH3:            uc.Type == ResolverTypeDOH3,
		transport:         uc.transport,
		http3RoundTripper: uc.http3RoundTripper,
	}
	return r
}

type dohResolver struct {
	endpoint          string
	isDoH3            bool
	transport         *http.Transport
	http3RoundTripper *http3.RoundTripper
}

func (r *dohResolver) Resolve(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	data, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	enc := base64.RawURLEncoding.EncodeToString(data)
	url := fmt.Sprintf("%s?dns=%s", r.endpoint, enc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	c := http.Client{Transport: r.transport}
	if r.isDoH3 {
		c.Transport = r.http3RoundTripper
	}
	resp, err := c.Do(req)
	if err != nil {
		if r.isDoH3 {
			r.http3RoundTripper.Close()
		}
		return nil, fmt.Errorf("could not perform request: %w", err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not read message from response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wrong response from DOH server, got: %s, status: %d", string(buf), resp.StatusCode)
	}

	answer := new(dns.Msg)
	return answer, answer.Unpack(buf)
}
