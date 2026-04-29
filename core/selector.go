package core

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
)

type Selector struct {
	cores map[string]Core
	nodes sync.Map
}

func NewSelector(c []conf.CoreConfig) (Core, error) {
	cs := make(map[string]Core, len(c))
	for _, t := range c {
		f, ok := cores[strings.ToLower(t.Type)]
		if !ok {
			return nil, errors.New("unknown core type: " + t.Type)
		}
		core1, err := f(&t)
		if err != nil {
			return nil, err
		}
		if t.Name == "" {
			cs[t.Type] = core1
		} else {
			cs[t.Name] = core1
		}
	}
	return &Selector{
		cores: cs,
	}, nil
}

func (s *Selector) Start() error {
	for i := range s.cores {
		err := s.cores[i].Start()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Selector) Close() error {
	var errs []error
	for i := range s.cores {
		if err := s.cores[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isSupported(protocol string, protocols []string) bool {
	for i := range protocols {
		if protocol == protocols[i] {
			return true
		}
	}
	return false
}

// validateRuntimeKey enforces the contract that any tag passed through the
// Selector is a runtime key in the form "coreType|logicalTag" with a known
// coreType. Single-core direct callers (xray.AddNode / sing.AddNode without
// the Selector) are NOT subject to this check — they treat the tag as an
// opaque string and don't depend on the "|" separator.
func validateRuntimeKey(rk string) (coreType, logicalTag string, err error) {
	coreType, logicalTag, ok := strings.Cut(rk, "|")
	if !ok {
		return "", "", fmt.Errorf("invalid runtime key %q: expected coreType|logicalTag", rk)
	}
	if coreType == "" {
		return "", "", fmt.Errorf("invalid runtime key %q: coreType is empty", rk)
	}
	if logicalTag == "" {
		return "", "", fmt.Errorf("invalid runtime key %q: logicalTag is empty", rk)
	}
	if coreType != "xray" && coreType != "sing" {
		return "", "", fmt.Errorf("invalid runtime key %q: unknown coreType %q (allowed: xray, sing)", rk, coreType)
	}
	return coreType, logicalTag, nil
}

func (s *Selector) AddNode(tag string, info *panel.NodeInfo, option *conf.Options) error {
	if _, _, err := validateRuntimeKey(tag); err != nil {
		return err
	}
	var core Core
	if len(option.CoreName) > 0 {
		// use name to select core
		if c, ok := s.cores[option.CoreName]; ok {
			core = c
		}
	} else {
		// use type to select core
		for _, c := range s.cores {
			if len(option.Core) == 0 {
				if !isSupported(info.Type, c.Protocols()) {
					continue
				}
			} else if option.Core != c.Type() {
				continue
			}
			core = c
		}
	}
	if core == nil {
		return errors.New("the node type is not support")
	}
	if len(option.Core) == 0 {
		option.Core = core.Type()
		err := option.UnmarshalJSON(option.RawOptions)
		if err != nil {
			return fmt.Errorf("unmarshal option error: %s", err)
		}
		option.RawOptions = nil
	}
	err := core.AddNode(tag, info, option)
	if err != nil {
		return err
	}
	s.nodes.Store(tag, core)
	return nil
}

func (s *Selector) DelNode(tag string) error {
	if _, _, err := validateRuntimeKey(tag); err != nil {
		return err
	}
	if t, e := s.nodes.Load(tag); e {
		err := t.(Core).DelNode(tag)
		if err != nil {
			return err
		}
		s.nodes.Delete(tag)
		return nil
	}
	return errors.New("the node is not have")
}

func (s *Selector) AddUsers(p *AddUsersParams) (added int, err error) {
	if _, _, err := validateRuntimeKey(p.Tag); err != nil {
		return 0, err
	}
	t, e := s.nodes.Load(p.Tag)
	if !e {
		return 0, errors.New("the node is not have")
	}
	return t.(Core).AddUsers(p)
}

func (s *Selector) GetUserTrafficSlice(tag string, reset bool) ([]panel.UserTraffic, error) {
	if _, _, err := validateRuntimeKey(tag); err != nil {
		return nil, err
	}
	t, e := s.nodes.Load(tag)
	if !e {
		return nil, errors.New("the node is not have")
	}
	return t.(Core).GetUserTrafficSlice(tag, reset)
}

func (s *Selector) AddUserTrafficSlice(tag string, traffic []panel.UserTraffic) error {
	if _, _, err := validateRuntimeKey(tag); err != nil {
		return err
	}
	t, e := s.nodes.Load(tag)
	if !e {
		return errors.New("the node is not have")
	}
	return t.(Core).AddUserTrafficSlice(tag, traffic)
}

func (s *Selector) DelUsers(users []panel.UserInfo, tag string) error {
	if _, _, err := validateRuntimeKey(tag); err != nil {
		return err
	}
	t, e := s.nodes.Load(tag)
	if !e {
		return errors.New("the node is not have")
	}
	return t.(Core).DelUsers(users, tag)
}

func (s *Selector) Protocols() []string {
	protocols := make([]string, 0)
	for i := range s.cores {
		protocols = append(protocols, s.cores[i].Protocols()...)
	}
	return protocols
}

func (s *Selector) Type() string {
	t := "Selector("
	var flag bool
	for n, c := range s.cores {
		if flag {
			t += " "
		} else {
			flag = true
		}
		if len(n) == 0 {
			t += c.Type()
		} else {
			t += n
		}
	}
	t += ")"
	return t
}
