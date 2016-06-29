package consumer

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudfoundry/noaa"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"
)

// RecentLogs connects to traffic controller via its 'recentlogs' http(s)
// endpoint and returns a slice of recent messages.  It does not guarantee any
// order of the messages; they are in the order returned by traffic controller.
//
// The noaa.SortRecent function is provided to sort the data returned by
// this method.
func (c *Consumer) RecentLogs(appGuid string, authToken string) ([]*events.LogMessage, error) {
	messages := make([]*events.LogMessage, 0, 200)
	callback := func(envelope *events.Envelope) error {
		messages = append(messages, envelope.GetLogMessage())
		return nil
	}
	err := c.readTC(appGuid, authToken, "recentlogs", callback)
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// ContainerMetrics connects to traffic controller via its 'containermetrics'
// http(s) endpoint and returns the most recent messages for an app.  The
// returned metrics will be sorted by InstanceIndex.
func (c *Consumer) ContainerMetrics(appGuid string, authToken string) ([]*events.ContainerMetric, error) {
	messages := make([]*events.ContainerMetric, 0, 200)
	callback := func(envelope *events.Envelope) error {
		if envelope.GetEventType() == events.Envelope_LogMessage {
			return errors.New(fmt.Sprintf("Upstream error: %s", envelope.GetLogMessage().GetMessage()))
		}
		messages = append(messages, envelope.GetContainerMetric())
		return nil
	}
	err := c.readTC(appGuid, authToken, "containermetrics", callback)
	if err != nil {
		return nil, err
	}
	noaa.SortContainerMetrics(messages)
	return messages, err
}

func (c *Consumer) readTC(appGuid string, authToken string, endpoint string, callback func(*events.Envelope) error) error {
	trafficControllerUrl, err := url.ParseRequestURI(c.trafficControllerUrl)
	if err != nil {
		return err
	}

	scheme := "https"
	if trafficControllerUrl.Scheme == "ws" {
		scheme = "http"
	}

	recentPath := fmt.Sprintf("%s://%s/apps/%s/%s", scheme, trafficControllerUrl.Host, appGuid, endpoint)

	resp, httpErr := c.tryTCConnection(recentPath, authToken)
	if httpErr != nil && httpErr.statusCode == http.StatusUnauthorized && c.refreshTokens {
		authToken, err = c.getToken()
		if httpErr != nil {
			return err
		}
		resp, httpErr = c.tryTCConnection(recentPath, authToken)
	}

	if httpErr != nil {
		return httpErr.error
	}

	defer resp.Body.Close()

	reader, err := getMultipartReader(resp)
	if err != nil {
		return err
	}

	var buffer bytes.Buffer

	for part, loopErr := reader.NextPart(); loopErr == nil; part, loopErr = reader.NextPart() {
		buffer.Reset()

		_, err = buffer.ReadFrom(part)
		if err != nil {
			break
		}

		envelope := new(events.Envelope)
		proto.Unmarshal(buffer.Bytes(), envelope)

		err = callback(envelope)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Consumer) tryTCConnection(recentPath, token string) (*http.Response, *httpError) {
	req, _ := http.NewRequest("GET", recentPath, nil)
	req.Header.Set("Authorization", token)

	resp, err := c.client.Do(req)
	if err != nil {
		message := `Error dialing traffic controller server: %s.
Please ask your Cloud Foundry Operator to check the platform configuration (traffic controller endpoint is %s).`
		return nil, &httpError{
			statusCode: -1,
			error:      errors.New(fmt.Sprintf(message, err, c.trafficControllerUrl)),
		}
	}

	return resp, checkForErrors(resp)
}

func getMultipartReader(resp *http.Response) (*multipart.Reader, error) {
	contentType := resp.Header.Get("Content-Type")

	if len(strings.TrimSpace(contentType)) == 0 {
		return nil, ErrBadResponse
	}

	matches := boundaryRegexp.FindStringSubmatch(contentType)

	if len(matches) != 2 || len(strings.TrimSpace(matches[1])) == 0 {
		return nil, ErrBadResponse
	}
	reader := multipart.NewReader(resp.Body, matches[1])
	return reader, nil
}
