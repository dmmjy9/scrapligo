package cfg

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/scrapli/scrapligo/logging"

	"github.com/scrapli/scrapligo/driver/base"
	"github.com/scrapli/scrapligo/driver/network"
)

type eosPatterns struct {
	globalCommentLinePattern *regexp.Regexp
	bannerPattern            *regexp.Regexp
	endPattern               *regexp.Regexp
}

var eosPatternsInstance *eosPatterns

func getEosPatterns() *eosPatterns {
	if eosPatternsInstance == nil {
		eosPatternsInstance = &eosPatterns{
			globalCommentLinePattern: regexp.MustCompile(`(?im)^! .*$`),
			bannerPattern:            regexp.MustCompile(`(?ims)^banner.*EOF$`),
			endPattern:               regexp.MustCompile(`end$`),
		}
	}

	return eosPatternsInstance
}

type EOSCfg struct {
	conn              *network.Driver
	VersionPattern    *regexp.Regexp
	configCommandMap  map[string]string
	configSessionName string
}

// NewEOSCfg return a cfg instance setup for an Arista EOS device.
func NewEOSCfg(
	conn *network.Driver,
	options ...Option,
) (*Cfg, error) {
	options = append([]Option{WithConfigSources([]string{"running", "startup"})}, options...)

	c, err := newCfg(conn, options...)
	if err != nil {
		return nil, err
	}

	c.Platform = &EOSCfg{
		conn:           conn,
		VersionPattern: regexp.MustCompile(`(?i)\d+\.\d+\.[a-z0-9\-]+(\.\d+[a-z]?)?`),
		configCommandMap: map[string]string{
			"running": "show running-config",
			"startup": "show startup-config",
		},
	}

	err = setPlatformOptions(c.Platform, options...)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (p *EOSCfg) ClearConfigSession() {
	p.configSessionName = ""
}

// GetVersion get the version from the device.
func (p *EOSCfg) GetVersion() (string, []*base.Response, error) {
	versionResult, err := p.conn.SendCommand("show version | i Software image version")
	if err != nil {
		return "", nil, err
	}

	return p.VersionPattern.FindString(versionResult.Result), []*base.Response{versionResult}, nil
}

func (p *EOSCfg) getConfigCommand(source string) (string, error) {
	cmd, ok := p.configCommandMap[source]

	if !ok {
		return "", ErrInvalidConfigTarget
	}

	return cmd, nil
}

// GetConfig get the configuration of a source datastore from the device.
func (p *EOSCfg) GetConfig(source string) (string, []*base.Response, error) {
	cmd, err := p.getConfigCommand(source)
	if err != nil {
		return "", nil, err
	}

	configResult, err := p.conn.SendCommand(cmd)

	if err != nil {
		return "", nil, err
	}

	return configResult.Result, []*base.Response{configResult}, nil
}

func (p *EOSCfg) prepareConfigPayloads(config string) (stdConfig, eagerConfig string) {
	patterns := getEosPatterns()

	// remove comment lines
	config = patterns.globalCommentLinePattern.ReplaceAllString(config, "!")

	// remove "end" at the end of the config - if its present it will drop scrapli out
	// of the config session which we do not want
	config = patterns.endPattern.ReplaceAllString(config, "!")

	// find all sections that need to be "eagerly" sent; remove those sections from the "normal"
	// config, then join all the eager sections into a single string
	eagerSections := patterns.bannerPattern.FindStringSubmatch(config)
	eagerConfig = strings.Join(eagerSections, "\n")

	for _, section := range eagerSections {
		config = strings.Replace(config, section, "!", -1)
	}

	return config, eagerConfig
}

// RegisterConfigSession register a configuration session in EOS.
func (p *EOSCfg) RegisterConfigSession(sessionName string) error {
	_, ok := p.conn.PrivilegeLevels[sessionName]

	if ok {
		return ErrConfigSessionAlreadyExists
	}

	sessionPrompt := regexp.QuoteMeta(sessionName[:6])
	sessionPromptPattern := fmt.Sprintf(
		`(?im)^[\w.\-@()/:\s]{1,63}\(config\-s\-%s[\w.\-@_/:]{0,32}\)#\s?$`,
		sessionPrompt,
	)

	sessionPrivilegeLevel := &base.PrivilegeLevel{
		Pattern:        sessionPromptPattern,
		Name:           sessionName,
		PreviousPriv:   "privilege_exec",
		Deescalate:     "end",
		Escalate:       fmt.Sprintf("configure session %s", sessionName),
		EscalateAuth:   false,
		EscalatePrompt: "",
	}

	p.conn.PrivilegeLevels[sessionName] = sessionPrivilegeLevel
	p.conn.UpdatePrivilegeLevels()

	return nil
}

func (p *EOSCfg) loadConfig(stdConfig, eagerConfig string, replace bool) ([]*base.Response, error) {
	var scrapliResponses []*base.Response

	if replace {
		rollbackCleanConfigResult, rollbackErr := p.conn.SendConfig("rollback clean-config",
			base.WithDesiredPrivilegeLevel(p.configSessionName))
		if rollbackErr != nil {
			return scrapliResponses, rollbackErr
		}

		scrapliResponses = append(scrapliResponses, rollbackCleanConfigResult)
	}

	configResult, stdConfigErr := p.conn.SendConfig(
		stdConfig,
		base.WithDesiredPrivilegeLevel(p.configSessionName),
	)
	if stdConfigErr != nil || configResult.Failed {
		return scrapliResponses, stdConfigErr
	}

	scrapliResponses = append(scrapliResponses, configResult)

	eagerResult, eagerConfigErr := p.conn.SendConfig(
		eagerConfig,
		base.WithDesiredPrivilegeLevel(p.configSessionName),
		base.WithSendEager(true),
	)
	if eagerConfigErr != nil {
		return scrapliResponses, eagerConfigErr
	} else if eagerResult.Failed {
		return scrapliResponses, eagerConfigErr
	}

	scrapliResponses = append(scrapliResponses, eagerResult)

	return scrapliResponses, nil
}

// LoadConfig load a candidate configuration.
func (p *EOSCfg) LoadConfig(
	config string,
	replace bool,
	options ...LoadOption,
) ([]*base.Response, error) {
	// options are unused for eos
	_ = options

	stdConfig, eagerConfig := p.prepareConfigPayloads(config)

	if p.configSessionName == "" {
		p.configSessionName = fmt.Sprintf("scrapli_cfg_%d", time.Now().Unix())

		logging.LogDebug(
			FormatLogMessage(
				p.conn,
				"debug",
				fmt.Sprintf("configuration session name will be %s", p.configSessionName),
			),
		)

		err := p.RegisterConfigSession(p.configSessionName)
		if err != nil {
			return nil, err
		}
	}

	return p.loadConfig(stdConfig, eagerConfig, replace)
}

// AbortConfig abort the loaded candidate configuration.
func (p *EOSCfg) AbortConfig() ([]*base.Response, error) {
	var scrapliResponses []*base.Response

	err := p.conn.AcquirePriv(p.configSessionName)
	if err != nil {
		return scrapliResponses, err
	}

	_, err = p.conn.Channel.SendInput("abort", false, false, p.conn.TimeoutOps)
	if err != nil {
		return scrapliResponses, err
	}

	p.conn.CurrentPriv = "privilege_exec"

	return nil, nil
}
