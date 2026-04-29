package panel

import (
	"fmt"

	"encoding/json/jsontext"
	"encoding/json/v2"

	log "github.com/sirupsen/logrus"
)

type OnlineUser struct {
	UID int    `json:"uid"`
	IP  string `json:"ip"`
}

type UserInfo struct {
	Id          int    `json:"id"`
	Uuid        string `json:"uuid"`
	SpeedLimit  int    `json:"speed_limit"`
	DeviceLimit int    `json:"device_limit"`
}

type UserListBody struct {
	Users []UserInfo `json:"users"`
}

type UserOnlineBody struct {
	Users []OnlineUser `json:"users"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

func (c *Client) GetUserList() ([]UserInfo, error) {
	const path = "/v1/server/user"
	r, err := c.Client.R().
		SetHeader("If-None-Match", c.userEtag).
		ForceContentType("application/json").
		SetDoNotParseResponse(true).
		Get(path)
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer r.RawResponse.Body.Close()

	if r.StatusCode() == 304 {
		return nil, nil
	}

	if err = c.checkResponse(r, path, err); err != nil {
		return nil, err
	}
	userlist := &UserListBody{}
	dec := jsontext.NewDecoder(r.RawResponse.Body)
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
		if tok.Kind() == '"' && tok.String() == "users" {
			break
		}
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, fmt.Errorf("decode user list error: %w", err)
	}
	if tok.Kind() != '[' {
		return nil, fmt.Errorf(`decode user list error: expected "users" array`)
	}
	for dec.PeekKind() != ']' {
		val, err := dec.ReadValue()
		if err != nil {
			return nil, fmt.Errorf("decode user list error: read user object: %w", err)
		}
		var u UserInfo
		if err := json.Unmarshal(val, &u); err != nil {
			return nil, fmt.Errorf("decode user list error: unmarshal user error: %w", err)
		}
		userlist.Users = append(userlist.Users, u)
	}
	c.userEtag = r.Header().Get("ETag")
	return userlist.Users, nil
}

func (c *Client) GetUserAlive() (map[int]int, error) {
	const path = "/v1/server/alivelist"
	c.AliveMap = &AliveMap{Alive: make(map[int]int)}

	r, err := c.Client.R().
		ForceContentType("application/json").
		Get(path)
	if err != nil || r == nil {
		log.WithFields(log.Fields{
			"path": path,
			"err":  err,
		}).Warn("alivelist request failed; falling back to empty map")
		return c.AliveMap.Alive, nil
	}
	if r.StatusCode() == 304 {
		return c.AliveMap.Alive, nil
	}
	if r.StatusCode() >= 400 {
		log.WithFields(log.Fields{
			"path":   path,
			"status": r.StatusCode(),
			"body":   string(r.Body()),
		}).Warn("alivelist returned non-2xx; falling back to empty map")
		return c.AliveMap.Alive, nil
	}
	body := &AliveMap{}
	if err := json.Unmarshal(r.Body(), body); err != nil {
		log.WithFields(log.Fields{
			"path": path,
			"err":  err,
		}).Warn("alivelist response unmarshal failed; falling back to empty map")
		return c.AliveMap.Alive, nil
	}
	if body.Alive == nil {
		log.WithField("path", path).Warn("alivelist response missing 'alive' field; falling back to empty map")
		return c.AliveMap.Alive, nil
	}
	c.AliveMap = body
	return body.Alive, nil
}

type ServerPushUserTrafficRequest struct {
	Traffic []UserTraffic `json:"traffic"`
}

type UserTraffic struct {
	UID      int   `json:"uid"`
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}

func (c *Client) ReportUserTraffic(userTraffic *[]UserTraffic) error {
	traffic := make([]UserTraffic, 0)
	for _, t := range *userTraffic {
		traffic = append(traffic, UserTraffic{
			UID:      t.UID,
			Upload:   t.Upload,
			Download: t.Download,
		})
	}
	path := "/v1/server/push"
	req := ServerPushUserTrafficRequest{
		Traffic: traffic,
	}
	r, err := c.Client.R().
		SetBody(req).
		ForceContentType("application/json").
		Post(path)
	err = c.checkResponse(r, path, err)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) ReportNodeOnlineUsers(data *[]OnlineUser) error {
	const path = "/v1/server/online"
	users := UserOnlineBody{
		Users: *data,
	}
	r, err := c.Client.R().
		SetBody(users).
		ForceContentType("application/json").
		Post(path)
	err = c.checkResponse(r, path, err)

	if err != nil {
		return nil
	}

	return nil
}
