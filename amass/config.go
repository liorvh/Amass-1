// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package amass

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/OWASP/Amass/amass/utils"
	"github.com/go-ini/ini"
)

const (
	defaultWordlistURL = "https://raw.githubusercontent.com/OWASP/Amass/master/wordlists/namelist.txt"
)

// Config passes along Amass enumeration configurations
type Config struct {
	sync.Mutex

	// The ports that will be checked for certificates
	Ports []int `ini:"port,,allowshadow"`

	// The list of words to use when generating names
	Wordlist []string

	// Will the enumeration including brute forcing techniques
	BruteForcing bool `ini:"brute_forcing"`

	// Will recursive brute forcing be performed?
	Recursive bool `ini:"recursive_brute_forcing"`

	// Minimum number of subdomain discoveries before performing recursive brute forcing
	MinForRecursive int `ini:"minimum_for_recursive"`

	// Will discovered subdomain name alterations be generated?
	Alterations bool `ini:"alterations"`

	// Indicates a speed band for the enumeration to execute within
	Timing EnumerationTiming

	// Only access the data sources for names and return results?
	Passive bool

	// Determines if zone transfers will be attempted
	Active bool

	// Determines if unresolved DNS names will be output by the enumeration
	IncludeUnresolvable bool `ini:"include_unresolvable"`

	// A blacklist of subdomain names that will not be investigated
	Blacklist []string

	// A list of data sources that should not be utilized
	DisabledDataSources []string

	// The root domain names that the enumeration will target
	domains []string

	// The regular expressions for the root domains added to the enumeration
	regexps map[string]*regexp.Regexp

	// The API keys used by various data sources
	apikeys map[string]*APIKey
}

// APIKey contains values required for authenticating with web APIs.
type APIKey struct {
	Username string `ini:"username"`
	Password string `ini:"password"`
	Key      string `ini:"apikey"`
	Secret   string `ini:"secret"`
}

// CheckSettings runs some sanity checks on the configuration options selected.
func (c *Config) CheckSettings() error {
	var err error

	if c.Passive && c.BruteForcing {
		return errors.New("Brute forcing cannot be performed without DNS resolution")
	}
	if c.Passive && c.Active {
		return errors.New("Active enumeration cannot be performed without DNS resolution")
	}
	if c.MinForRecursive == 0 {
		c.MinForRecursive = 1
	}
	if len(c.Ports) == 0 {
		c.Ports = []int{443}
	}
	if len(c.Wordlist) == 0 {
		c.Wordlist, err = getDefaultWordlist()
	}
	return err
}

// DomainRegex returns the Regexp object for the domain name identified by the parameter.
func (c *Config) DomainRegex(domain string) *regexp.Regexp {
	c.Lock()
	defer c.Unlock()

	if re, found := c.regexps[domain]; found {
		return re
	}
	return nil
}

// AddDomain appends the domain name provided in the parameter to the list in the configuration.
func (c *Config) AddDomain(domain string) {
	c.Lock()
	defer c.Unlock()

	d := strings.TrimSpace(domain)
	if d == "" {
		return
	}

	c.domains = utils.UniqueAppend(c.domains, d)
	if c.regexps == nil {
		c.regexps = make(map[string]*regexp.Regexp)
	}
	c.regexps[d] = utils.SubdomainRegex(d)
}

// Domains returns the list of domain names currently in the configuration.
func (c *Config) Domains() []string {
	c.Lock()
	defer c.Unlock()

	return c.domains
}

// IsDomainInScope returns true if the DNS name in the parameter ends with a domain in the config list.
func (c *Config) IsDomainInScope(name string) bool {
	var discovered bool

	n := strings.TrimSpace(name)
	for _, d := range c.Domains() {
		if n == d || strings.HasSuffix(n, "."+d) {
			discovered = true
			break
		}
	}
	return discovered
}

// WhichDomain returns the domain in the config list that the DNS name in the parameter end with.
func (c *Config) WhichDomain(name string) string {
	n := strings.TrimSpace(name)

	for _, d := range c.Domains() {
		if n == d || strings.HasSuffix(n, "."+d) {
			return d
		}
	}
	return ""
}

// Blacklisted returns true is the name in the parameter ends with a subdomain name in the config blacklist.
func (c *Config) Blacklisted(name string) bool {
	var resp bool

	n := strings.TrimSpace(name)
	for _, bl := range c.Blacklist {
		if match := strings.HasSuffix(n, bl); match {
			resp = true
			break
		}
	}
	return resp
}

// ExcludeDisabledDataSources returns a list of data sources excluding DisabledDataSources.
func (c *Config) ExcludeDisabledDataSources(services []Service) []Service {
	var enabled []Service

	for _, s := range services {
		include := true

		for _, disabled := range c.DisabledDataSources {
			if strings.EqualFold(disabled, s.String()) {
				include = false
				break
			}
		}
		if include {
			enabled = append(enabled, s)
		}
	}
	return enabled
}

// AddAPIKey adds the data source and API key association provided to the configuration.
func (c *Config) AddAPIKey(source string, ak *APIKey) {
	c.Lock()
	defer c.Unlock()

	idx := strings.TrimSpace(source)
	if idx == "" {
		return
	}

	if c.apikeys == nil {
		c.apikeys = make(map[string]*APIKey)
	}
	c.apikeys[strings.ToLower(idx)] = ak
}

// GetAPIKey returns the API key associated with the provided data source name.
func (c *Config) GetAPIKey(source string) *APIKey {
	c.Lock()
	defer c.Unlock()

	idx := strings.TrimSpace(source)
	if apikey, found := c.apikeys[strings.ToLower(idx)]; found {
		return apikey
	}
	return nil
}

// LoadSettings parses settings from an .ini file and assigns them to the Config.
func (c *Config) LoadSettings(path string) error {
	cfg, err := ini.LoadSources(ini.LoadOptions{
		Insensitive:  true,
		AllowShadows: true,
	}, path)
	if err != nil {
		return fmt.Errorf("Failed to load the configuration file: %v", err)
	}
	// Get the easy ones out of the way using mapping
	if err = cfg.MapTo(c); err != nil {
		return fmt.Errorf("Error mapping configuration settings to internal values: %v", err)
	}
	// Attempt to load a wordlist provided via the configuration file
	if cfg.Section(ini.DEFAULT_SECTION).HasKey("wordlist_file") {
		wordlist := cfg.Section(ini.DEFAULT_SECTION).Key("wordlist_file").String()

		list, err := getWordList(wordlist)
		if err != nil {
			return fmt.Errorf("Unable to load the file in the wordlist_file setting: %s: %v", wordlist, err)
		}
		c.Wordlist = list
	}
	// Attempt to load the timing setting via the configuration file
	if cfg.Section(ini.DEFAULT_SECTION).HasKey("timing") {
		tstr := cfg.Section(ini.DEFAULT_SECTION).Key("timing").String()

		timing, err := strconv.Atoi(tstr)
		if err != nil || timing < 0 || timing > 5 {
			return fmt.Errorf("%s is not a valid enumeration timing value", tstr)
		}
		c.Timing = EnumerationTiming(timing)
	}
	// Attempt to load a special mode of operation specified by the user
	if cfg.Section(ini.DEFAULT_SECTION).HasKey("mode") {
		mode := cfg.Section(ini.DEFAULT_SECTION).Key("mode").String()

		if mode == "passive" {
			c.Passive = true
		} else if mode == "active" {
			c.Active = true
		}
	}
	// Load up all the DNS domain names
	if domains, err := cfg.GetSection("domains"); err == nil {
		for _, domain := range domains.Key("domain").ValueWithShadows() {
			c.AddDomain(domain)
		}
	}
	// Load up all the blacklisted subdomain names
	if blacklisted, err := cfg.GetSection("blacklisted"); err == nil {
		c.Blacklist = utils.UniqueAppend(c.Blacklist,
			blacklisted.Key("subdomain").ValueWithShadows()...)
	}
	// Load up all the disabled data source names
	if disabled, err := cfg.GetSection("disabled_data_sources"); err == nil {
		c.DisabledDataSources = utils.UniqueAppend(
			c.DisabledDataSources, disabled.Key("data_source").ValueWithShadows()...)
	}
	// Load up all API key information from data source sections
	for _, section := range cfg.Sections() {
		name := section.Name()
		// Skip sections that are not related to data sources
		if name == "default" || name == "domains" || name == "resolvers" ||
			name == "blacklisted" || name == "disabled_data_sources" {
			continue
		}

		key := new(APIKey)
		// Parse the API key information and assign to the Config
		if err := section.MapTo(key); err == nil {
			c.AddAPIKey(name, key)
		}
	}
	return nil
}

// GetResolversFromSettings loads the configuration file and returns all resolvers found.
func GetResolversFromSettings(path string) ([]string, error) {
	cfg, err := ini.LoadSources(ini.LoadOptions{
		Insensitive:  true,
		AllowShadows: true,
	}, path)
	if err != nil {
		return nil, fmt.Errorf("Failed to load the configuration file: %v", err)
	}
	// Check that the resolvers section exists in the config file
	sec, err := cfg.GetSection("resolvers")
	if err != nil {
		return nil, fmt.Errorf("The config file does not contain a resolvers section: %v", err)
	}

	resolvers := sec.Key("resolver").ValueWithShadows()
	if len(resolvers) == 0 {
		return nil, errors.New("No resolver keys were found in the resolvers section")
	}
	return resolvers, nil
}

func getWordList(path string) ([]string, error) {
	var lines []string
	var reader io.Reader

	// Open the file
	file, err := os.Open(path)
	if err != nil {
		return lines, fmt.Errorf("Error opening the file %s: %v", path, err)
	}
	defer file.Close()
	reader = file

	// We need to determine if this is a gzipped file or a plain text file, so we
	// first read the first 512 bytes to pass them down to http.DetectContentType
	// for mime detection. The file is rewinded before passing it along to the
	// next reader
	head := make([]byte, 512)
	if _, err = file.Read(head); err != nil {
		return lines, fmt.Errorf("Error reading the first 512 bytes from %s: %s", path, err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		return lines, fmt.Errorf("Error rewinding the file %s: %s", path, err)
	}

	// Read the file as gzip if it's actually compressed
	if mt := http.DetectContentType(head); mt == "application/gzip" || mt == "application/x-gzip" {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return lines, fmt.Errorf("Error gz-reading the file %s: %v", path, err)
		}
		defer gzReader.Close()
		reader = gzReader
	}
	// Get each line from the file
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		// Get the next line
		text := scanner.Text()
		if text != "" {
			lines = append(lines, strings.TrimSpace(text))
		}
	}
	return lines, nil
}

func getDefaultWordlist() ([]string, error) {
	var list []string

	page, err := utils.RequestWebPage(defaultWordlistURL, nil, nil, "", "")
	if err != nil {
		return list, err
	}

	scanner := bufio.NewScanner(strings.NewReader(page))
	for scanner.Scan() {
		// Get the next word in the list
		word := strings.TrimSpace(scanner.Text())
		if err := scanner.Err(); err == nil && word != "" {
			list = utils.UniqueAppend(list, word)
		}
	}
	return list, nil
}
