// Package controller provides a client for the controller API.
package controller

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/controller/utils"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/discoverd/client/dialer"
	"github.com/flynn/flynn/pkg/pinned"
	"github.com/flynn/flynn/pkg/rpcplus"
	"github.com/flynn/flynn/router/types"
)

// NewClient creates a new Client pointing at uri and using key for
// authentication.
func NewClient(uri, key string) (*Client, error) {
	if uri == "" {
		uri = "discoverd+http://flynn-controller"
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	c := &Client{
		url:  uri,
		addr: u.Host,
		HTTP: http.DefaultClient,
		key:  key,
	}
	if u.Scheme == "discoverd+http" {
		if err := discoverd.Connect(""); err != nil {
			return nil, err
		}
		dialer := dialer.New(discoverd.DefaultClient, nil)
		c.dial = dialer.Dial
		c.dialClose = dialer
		c.HTTP = &http.Client{Transport: &http.Transport{Dial: c.dial}}
		u.Scheme = "http"
		c.url = u.String()
	}
	return c, nil
}

// NewClientWithPin acts like NewClient, but specifies a TLS pin.
func NewClientWithPin(uri, key string, pin []byte) (*Client, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	c := &Client{
		dial: (&pinned.Config{Pin: pin}).Dial,
		key:  key,
	}
	if _, port, _ := net.SplitHostPort(u.Host); port == "" {
		u.Host += ":443"
	}
	c.addr = u.Host
	u.Scheme = "http"
	c.url = u.String()
	c.HTTP = &http.Client{Transport: &http.Transport{Dial: c.dial}}
	return c, nil
}

// Client is a client for the controller API.
type Client struct {
	url  string
	key  string
	addr string
	HTTP *http.Client

	dial      rpcplus.DialFunc
	dialClose io.Closer
}

// Close closes the underlying transport connection.
func (c *Client) Close() error {
	if c.dialClose != nil {
		c.dialClose.Close()
	}
	return nil
}

// ErrNotFound is returned when a resource is not found (HTTP status 404).
var ErrNotFound = errors.New("controller: not found")

func toJSON(v interface{}) (io.Reader, error) {
	data, err := json.Marshal(v)
	return bytes.NewBuffer(data), err
}

func (c *Client) rawReq(method, path string, header http.Header, in, out interface{}) (*http.Response, error) {
	var payload io.Reader
	switch v := in.(type) {
	case io.Reader:
		payload = v
	case nil:
	default:
		var err error
		payload, err = toJSON(in)
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(method, c.url+path, payload)
	if err != nil {
		return nil, err
	}
	if header == nil {
		header = make(http.Header)
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/json")
	}
	req.Header = header
	req.SetBasicAuth("", c.key)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == 404 {
		res.Body.Close()
		return res, ErrNotFound
	}
	if res.StatusCode == 400 {
		var body ct.ValidationError
		defer res.Body.Close()
		if err = json.NewDecoder(res.Body).Decode(&body); err != nil {
			return res, err
		}
		return res, body
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return res, &url.Error{
			Op:  req.Method,
			URL: req.URL.String(),
			Err: fmt.Errorf("controller: unexpected status %d", res.StatusCode),
		}
	}
	if out != nil {
		defer res.Body.Close()
		return res, json.NewDecoder(res.Body).Decode(out)
	}
	return res, nil
}

func (c *Client) send(method, path string, in, out interface{}) error {
	_, err := c.rawReq(method, path, nil, in, out)
	return err
}

func (c *Client) put(path string, in, out interface{}) error {
	return c.send("PUT", path, in, out)
}

func (c *Client) post(path string, in, out interface{}) error {
	return c.send("POST", path, in, out)
}

func (c *Client) get(path string, out interface{}) error {
	_, err := c.rawReq("GET", path, nil, nil, out)
	return err
}

func (c *Client) delete(path string) error {
	res, err := c.rawReq("DELETE", path, nil, nil, nil)
	if err == nil {
		res.Body.Close()
	}
	return err
}

// FormationUpdates is a wrapper around a Chan channel, allowing us to close
// the stream.
type FormationUpdates struct {
	Chan <-chan *ct.ExpandedFormation

	conn net.Conn
}

// Close closes the underlying stream.
func (u *FormationUpdates) Close() error {
	if u.conn == nil {
		return nil
	}
	return u.conn.Close()
}

// StreamFormations returns a FormationUpdates stream. If since is not nil, only
// retrieves formation updates since the specified time.
func (c *Client) StreamFormations(since *time.Time) (*FormationUpdates, *error) {
	if since == nil {
		s := time.Unix(0, 0)
		since = &s
	}
	dial := c.dial
	if dial == nil {
		dial = net.Dial
	}
	ch := make(chan *ct.ExpandedFormation)
	conn, err := dial("tcp", c.addr)
	if err != nil {
		close(ch)
		return &FormationUpdates{ch, conn}, &err
	}
	header := make(http.Header)
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+c.key)))
	client, err := rpcplus.NewHTTPClient(conn, rpcplus.DefaultRPCPath, header)
	if err != nil {
		close(ch)
		return &FormationUpdates{ch, conn}, &err
	}
	return &FormationUpdates{ch, conn}, &client.StreamGo("Controller.StreamFormations", since, ch).Error
}

// CreateArtifact creates a new artifact.
func (c *Client) CreateArtifact(artifact *ct.Artifact) error {
	return c.post("/artifacts", artifact, artifact)
}

// CreateRelease creates a new release.
func (c *Client) CreateRelease(release *ct.Release) error {
	return c.post("/releases", release, release)
}

// CreateApp creates a new app.
func (c *Client) CreateApp(app *ct.App) error {
	return c.post("/apps", app, app)
}

// DeleteApp deletes an app.
func (c *Client) DeleteApp(appID string) error {
	return c.delete(fmt.Sprintf("/apps/%s", appID))
}

// CreateProvider cretes a new provider.
func (c *Client) CreateProvider(provider *ct.Provider) error {
	return c.post("/providers", provider, provider)
}

func (c *Client) GetProvider(providerID string) (*ct.Provider, error) {
	provider := &ct.Provider{}
	return provider, c.get(fmt.Sprintf("/providers/%s", providerID), provider)
}

// ProvisionResource uses a provider to provision a new resource for the
// application. Returns details about the resource.
func (c *Client) ProvisionResource(req *ct.ResourceReq) (*ct.Resource, error) {
	if req.ProviderID == "" {
		return nil, errors.New("controller: missing provider id")
	}
	res := &ct.Resource{}
	err := c.post(fmt.Sprintf("/providers/%s/resources", req.ProviderID), req, res)
	return res, err
}

func (c *Client) GetResource(providerID, resourceID string) (*ct.Resource, error) {
	res := &ct.Resource{}
	err := c.get(fmt.Sprintf("/providers/%s/resources/%s", providerID, resourceID), res)
	return res, err
}

// PutResource updates a resource.
func (c *Client) PutResource(resource *ct.Resource) error {
	if resource.ID == "" || resource.ProviderID == "" {
		return errors.New("controller: missing id and/or provider id")
	}
	return c.put(fmt.Sprintf("/providers/%s/resources/%s", resource.ProviderID, resource.ID), resource, resource)
}

// PutFormation updates an existing formation.
func (c *Client) PutFormation(formation *ct.Formation) error {
	if formation.AppID == "" || formation.ReleaseID == "" {
		return errors.New("controller: missing app id and/or release id")
	}
	return c.put(fmt.Sprintf("/apps/%s/formations/%s", formation.AppID, formation.ReleaseID), formation, formation)
}

// PutJob updates an existing job.
func (c *Client) PutJob(job *ct.Job) error {
	if job.ID == "" || job.AppID == "" {
		return errors.New("controller: missing job id and/or app id")
	}
	return c.put(fmt.Sprintf("/apps/%s/jobs/%s", job.AppID, job.ID), job, job)
}

// DeleteJob kills a specific job id under the specified app.
func (c *Client) DeleteJob(appID, jobID string) error {
	return c.delete(fmt.Sprintf("/apps/%s/jobs/%s", appID, jobID))
}

// SetAppRelease sets the specified release as the current release for an app.
func (c *Client) SetAppRelease(appID, releaseID string) error {
	return c.put(fmt.Sprintf("/apps/%s/release", appID), &ct.Release{ID: releaseID}, nil)
}

// GetAppRelease returns the current release of an app.
func (c *Client) GetAppRelease(appID string) (*ct.Release, error) {
	release := &ct.Release{}
	return release, c.get(fmt.Sprintf("/apps/%s/release", appID), release)
}

// RouteList returns all routes for an app.
func (c *Client) RouteList(appID string) ([]*router.Route, error) {
	var routes []*router.Route
	return routes, c.get(fmt.Sprintf("/apps/%s/routes", appID), &routes)
}

// GetRoute returns details for the routeID under the specified app.
func (c *Client) GetRoute(appID string, routeID string) (*router.Route, error) {
	route := &router.Route{}
	return route, c.get(fmt.Sprintf("/apps/%s/routes/%s", appID, routeID), route)
}

// CreateRoute creates a new route for the specified app.
func (c *Client) CreateRoute(appID string, route *router.Route) error {
	return c.post(fmt.Sprintf("/apps/%s/routes", appID), route, route)
}

// DeleteRoute deletes a route under the specified app.
func (c *Client) DeleteRoute(appID string, routeID string) error {
	return c.delete(fmt.Sprintf("/apps/%s/routes/%s", appID, routeID))
}

// GetFormation returns details for the specified formation under app and
// release.
func (c *Client) GetFormation(appID, releaseID string) (*ct.Formation, error) {
	formation := &ct.Formation{}
	return formation, c.get(fmt.Sprintf("/apps/%s/formations/%s", appID, releaseID), formation)
}

// GetRelease returns details for the specified release.
func (c *Client) GetRelease(releaseID string) (*ct.Release, error) {
	release := &ct.Release{}
	return release, c.get(fmt.Sprintf("/releases/%s", releaseID), release)
}

// GetArtifact returns details for the specified artifact.
func (c *Client) GetArtifact(artifactID string) (*ct.Artifact, error) {
	artifact := &ct.Artifact{}
	return artifact, c.get(fmt.Sprintf("/artifacts/%s", artifactID), artifact)
}

// GetApp returns details for the specified app.
func (c *Client) GetApp(appID string) (*ct.App, error) {
	app := &ct.App{}
	return app, c.get(fmt.Sprintf("/apps/%s", appID), app)
}

type sseDecoder struct {
	*bufio.Reader
}

// Decode finds the next "data" field and decodes it into v
func (dec *sseDecoder) Decode(v interface{}) error {
	for {
		line, err := dec.ReadBytes('\n')
		if err != nil {
			return err
		}
		if bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimPrefix(line, []byte("data: "))
			return json.Unmarshal(data, v)
		}
	}
}

// JobEventStream is a wrapper around an Events channel, allowing us to close
// the stream.
type JobEventStream struct {
	Events chan *ct.JobEvent
	body   io.ReadCloser
}

// Close closes the underlying stream.
func (s *JobEventStream) Close() {
	s.body.Close()
}

// StreamJobEvents returns a JobEventStream for an app.
func (c *Client) StreamJobEvents(appID string) (*JobEventStream, error) {
	res, err := c.rawReq("GET", fmt.Sprintf("/apps/%s/jobs", appID), http.Header{"Accept": []string{"text/event-stream"}}, nil, nil)
	if err != nil {
		return nil, err
	}
	stream := &JobEventStream{Events: make(chan *ct.JobEvent), body: res.Body}
	go func() {
		defer close(stream.Events)
		dec := &sseDecoder{bufio.NewReader(stream.body)}
		for {
			event := &ct.JobEvent{}
			if err := dec.Decode(event); err != nil {
				return
			}
			stream.Events <- event
		}
	}()
	return stream, nil
}

// GetJobLog returns a ReadCloser stream of the job with id of jobID, running
// under appID. If tail is true, new log lines are streamed after the buffered
// log.
func (c *Client) GetJobLog(appID, jobID string, tail bool) (io.ReadCloser, error) {
	path := fmt.Sprintf("/apps/%s/jobs/%s/log", appID, jobID)
	if tail {
		path += "?tail=true"
	}
	res, err := c.rawReq("GET", path, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// RunJobAttached runs a new job under the specified app, attaching to the job
// and returning a ReadWriteCloser stream, which can then be used for
// communicating with the job.
func (c *Client) RunJobAttached(appID string, job *ct.NewJob) (utils.ReadWriteCloser, error) {
	data, err := toJSON(job)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/apps/%s/jobs", c.url, appID), data)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.flynn.attach")
	req.SetBasicAuth("", c.key)
	var dial rpcplus.DialFunc
	if c.dial != nil {
		dial = c.dial
	}
	res, rwc, err := utils.HijackRequest(req, dial)
	if err != nil {
		res.Body.Close()
		return nil, err
	}
	return rwc, nil
}

// RunJobDetached runs a new job under the specified app, returning the job's
// details.
func (c *Client) RunJobDetached(appID string, req *ct.NewJob) (*ct.Job, error) {
	job := &ct.Job{}
	return job, c.post(fmt.Sprintf("/apps/%s/jobs", appID), req, job)
}

// JobList returns a list of all jobs.
func (c *Client) JobList(appID string) ([]*ct.Job, error) {
	var jobs []*ct.Job
	return jobs, c.get(fmt.Sprintf("/apps/%s/jobs", appID), &jobs)
}

// AppList returns a list of all apps.
func (c *Client) AppList() ([]*ct.App, error) {
	var apps []*ct.App
	return apps, c.get("/apps", &apps)
}

// KeyList returns a list of all ssh public keys added.
func (c *Client) KeyList() ([]*ct.Key, error) {
	var keys []*ct.Key
	return keys, c.get("/keys", &keys)
}

func (c *Client) ArtifactList() ([]*ct.Artifact, error) {
	var artifacts []*ct.Artifact
	return artifacts, c.get("/artifacts", &artifacts)
}

func (c *Client) ReleaseList() ([]*ct.Release, error) {
	var releases []*ct.Release
	return releases, c.get("/releases", &releases)
}

// CreateKey uploads pubKey as the ssh public key.
func (c *Client) CreateKey(pubKey string) (*ct.Key, error) {
	key := &ct.Key{}
	return key, c.post("/keys", &ct.Key{Key: pubKey}, key)
}

func (c *Client) GetKey(keyID string) (*ct.Key, error) {
	key := &ct.Key{}
	return key, c.get(fmt.Sprintf("/keys/%s", keyID), key)
}

// DeleteKey deletes a key with the specified id.
func (c *Client) DeleteKey(id string) error {
	return c.delete("/keys/" + strings.Replace(id, ":", "", -1))
}

// ProviderList returns a list of all providers.
func (c *Client) ProviderList() ([]*ct.Provider, error) {
	var providers []*ct.Provider
	return providers, c.get("/providers", &providers)
}
