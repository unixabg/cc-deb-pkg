/*
Copyright 2015 Google Inc. All rights reserved.

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codegangsta/cli"
	"github.com/google/cups-connector/gcp"
	"github.com/google/cups-connector/lib"

	"golang.org/x/oauth2"
)

const (
	gcpOAuthDeviceCodeURL   = "https://accounts.google.com/o/oauth2/device/code"
	gcpOAuthTokenPollURL    = "https://www.googleapis.com/oauth2/v3/token"
	gcpOAuthGrantTypeDevice = "http://oauth.net/grant_type/device/1.0"
)

var initFlags = []cli.Flag{
	cli.StringFlag{
		Name:  "gcp-user-refresh-token",
		Usage: "GCP user refresh token, useful when managing many connectors",
	},
	cli.DurationFlag{
		Name:  "gcp-api-timeout",
		Usage: "GCP API timeout, for debugging",
		Value: 30 * time.Second,
	},

	cli.StringFlag{
		Name:  "share-scope",
		Usage: "Scope (user or group email address) to automatically share printers with",
	},
	cli.StringFlag{
		Name:  "proxy-name",
		Usage: "Name for this connector instance. Should be unique per Google user account",
	},
	cli.IntFlag{
		Name:  "xmpp-port",
		Usage: "Max connections to CUPS server",
		Value: int(lib.DefaultConfig.XMPPPort),
	},
	cli.StringFlag{
		Name:  "gcp-xmpp-ping-timeout",
		Usage: "GCP XMPP ping timeout (give up waiting for ping response after this)",
		Value: lib.DefaultConfig.XMPPPingTimeout,
	},
	cli.StringFlag{
		Name:  "gcp-xmpp-ping-interval-default",
		Usage: "GCP XMPP ping interval default (ping every this often)",
		Value: lib.DefaultConfig.XMPPPingInterval,
	},
	cli.IntFlag{
		Name:  "gcp-max-concurrent-downloads",
		Usage: "Maximum quantity of PDFs to download concurrently from GCP cloud service",
		Value: int(lib.DefaultConfig.GCPMaxConcurrentDownloads),
	},
	cli.IntFlag{
		Name:  "cups-max-connections",
		Usage: "Max connections to CUPS server",
		Value: int(lib.DefaultConfig.CUPSMaxConnections),
	},
	cli.StringFlag{
		Name:  "cups-connect-timeout",
		Usage: "CUPS timeout for opening a new connection",
		Value: lib.DefaultConfig.CUPSConnectTimeout,
	},
	cli.IntFlag{
		Name:  "cups-job-queue-size",
		Usage: "CUPS job queue size",
		Value: int(lib.DefaultConfig.CUPSJobQueueSize),
	},
	cli.StringFlag{
		Name:  "cups-printer-poll-interval",
		Usage: "Interval, in seconds, between CUPS printer state polls",
		Value: lib.DefaultConfig.CUPSPrinterPollInterval,
	},
	cli.BoolFlag{
		Name:  "cups-job-full-username",
		Usage: "Whether to use the full username (joe@example.com) in CUPS jobs",
	},
	cli.BoolTFlag{
		Name:  "cups-ignore-raw-printers",
		Usage: "Whether to ignore CUPS raw printers",
	},
	cli.BoolTFlag{
		Name:  "copy-printer-info-to-display-name",
		Usage: "Whether to copy the CUPS printer's printer-info attribute to the GCP printer's defaultDisplayName",
	},
	cli.BoolFlag{
		Name:  "prefix-job-id-to-job-title",
		Usage: "Whether to add the job ID to the beginning of the job title",
	},
	cli.StringFlag{
		Name:  "display-name-prefix",
		Usage: "Prefix to add to GCP printer's display name",
		Value: lib.DefaultConfig.DisplayNamePrefix,
	},
	cli.StringFlag{
		Name:  "monitor-socket-filename",
		Usage: "Filename of unix socket for connector-check to talk to connector",
		Value: lib.DefaultConfig.MonitorSocketFilename,
	},
	cli.BoolFlag{
		Name:  "snmp-enable",
		Usage: "SNMP enable",
	},
	cli.StringFlag{
		Name:  "snmp-community",
		Usage: "SNMP community (usually \"public\")",
		Value: lib.DefaultConfig.SNMPCommunity,
	},
	cli.IntFlag{
		Name:  "snmp-max-connections",
		Usage: "Max connections to SNMP agents",
		Value: int(lib.DefaultConfig.SNMPMaxConnections),
	},
	cli.BoolFlag{
		Name:  "local-printing-enable",
		Usage: "Enable local discovery and printing (aka GCP 2.0 or Privet)",
	},
	cli.BoolFlag{
		Name:  "cloud-printing-enable",
		Usage: "Enable cloud discovery and printing",
	},
	cli.StringFlag{
		Name:  "log-file-name",
		Usage: "Log file name, full path",
		Value: lib.DefaultConfig.LogFileName,
	},
	cli.IntFlag{
		Name:  "log-file-max-megabytes",
		Usage: "Log file max size, in megabytes",
		Value: int(lib.DefaultConfig.LogFileMaxMegabytes),
	},
	cli.IntFlag{
		Name:  "log-max-files",
		Usage: "Maximum log file quantity before rollover",
		Value: int(lib.DefaultConfig.LogMaxFiles),
	},
	cli.StringFlag{
		Name:  "log-level",
		Usage: "Minimum event severity to log: PANIC, ERROR, WARN, INFO, DEBUG, VERBOSE",
		Value: lib.DefaultConfig.LogLevel,
	},
}

// getUserClientFromUser follows the token acquisition steps outlined here:
// https://developers.google.com/identity/protocols/OAuth2ForDevices
func getUserClientFromUser(context *cli.Context) (*http.Client, string) {
	form := url.Values{
		"client_id": {lib.DefaultConfig.GCPOAuthClientID},
		"scope":     {gcp.ScopeCloudPrint},
	}
	response, err := http.PostForm(gcpOAuthDeviceCodeURL, form)
	if err != nil {
		log.Fatalln(err)
	}

	var r struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	json.NewDecoder(response.Body).Decode(&r)

	fmt.Printf("Visit %s, and enter this code. I'll wait for you.\n%s\n",
		r.VerificationURL, r.UserCode)

	return pollOAuthConfirmation(context, r.DeviceCode, r.Interval)
}

func pollOAuthConfirmation(context *cli.Context, deviceCode string, interval int) (*http.Client, string) {
	config := oauth2.Config{
		ClientID:     lib.DefaultConfig.GCPOAuthClientID,
		ClientSecret: lib.DefaultConfig.GCPOAuthClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  lib.DefaultConfig.GCPOAuthAuthURL,
			TokenURL: lib.DefaultConfig.GCPOAuthTokenURL,
		},
		RedirectURL: gcp.RedirectURL,
		Scopes:      []string{gcp.ScopeCloudPrint},
	}

	for {
		time.Sleep(time.Duration(interval) * time.Second)

		form := url.Values{
			"client_id":     {lib.DefaultConfig.GCPOAuthClientID},
			"client_secret": {lib.DefaultConfig.GCPOAuthClientSecret},
			"code":          {deviceCode},
			"grant_type":    {gcpOAuthGrantTypeDevice},
		}
		response, err := http.PostForm(gcpOAuthTokenPollURL, form)
		if err != nil {
			log.Fatalln(err)
		}

		var r struct {
			Error        string `json:"error"`
			AccessToken  string `json:"access_token"`
			ExpiresIn    int    `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		}
		json.NewDecoder(response.Body).Decode(&r)

		switch r.Error {
		case "":
			token := &oauth2.Token{RefreshToken: r.RefreshToken}
			client := config.Client(oauth2.NoContext, token)
			client.Timeout = context.Duration("gcp-api-timeout")
			return client, r.RefreshToken
		case "authorization_pending":
		case "slow_down":
			interval *= 2
		default:
			log.Fatalln(err)
		}
	}

	panic("unreachable")
}

// getUserClientFromToken creates a user client with just a refresh token.
func getUserClientFromToken(context *cli.Context) *http.Client {
	config := &oauth2.Config{
		ClientID:     lib.DefaultConfig.GCPOAuthClientID,
		ClientSecret: lib.DefaultConfig.GCPOAuthClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  lib.DefaultConfig.GCPOAuthAuthURL,
			TokenURL: lib.DefaultConfig.GCPOAuthTokenURL,
		},
		RedirectURL: gcp.RedirectURL,
		Scopes:      []string{gcp.ScopeCloudPrint},
	}

	token := &oauth2.Token{RefreshToken: context.String("gcp-user-refresh-token")}
	client := config.Client(oauth2.NoContext, token)
	client.Timeout = context.Duration("gcp-api-timeout")

	return client
}

// initRobotAccount creates a GCP robot account for this connector.
func initRobotAccount(context *cli.Context, userClient *http.Client) (string, string) {
	params := url.Values{}
	params.Set("oauth_client_id", lib.DefaultConfig.GCPOAuthClientID)

	url := fmt.Sprintf("%s%s?%s", lib.DefaultConfig.GCPBaseURL, "createrobot", params.Encode())
	response, err := userClient.Get(url)
	if err != nil {
		log.Fatalln(err)
	}
	if response.StatusCode != http.StatusOK {
		log.Fatalf("Failed to initialize robot account: %s\n", response.Status)
	}

	var robotInit struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		XMPPJID  string `json:"xmpp_jid"`
		AuthCode string `json:"authorization_code"`
	}

	if err = json.NewDecoder(response.Body).Decode(&robotInit); err != nil {
		log.Fatalln(err)
	}
	if !robotInit.Success {
		log.Fatalf("Failed to initialize robot account: %s\n", robotInit.Message)
	}

	return robotInit.XMPPJID, robotInit.AuthCode
}

func verifyRobotAccount(authCode string) string {
	config := &oauth2.Config{
		ClientID:     lib.DefaultConfig.GCPOAuthClientID,
		ClientSecret: lib.DefaultConfig.GCPOAuthClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  lib.DefaultConfig.GCPOAuthAuthURL,
			TokenURL: lib.DefaultConfig.GCPOAuthTokenURL,
		},
		RedirectURL: gcp.RedirectURL,
		Scopes:      []string{gcp.ScopeCloudPrint, gcp.ScopeGoogleTalk},
	}

	token, err := config.Exchange(oauth2.NoContext, authCode)
	if err != nil {
		log.Fatalln(err)
	}

	return token.RefreshToken
}

func createRobotAccount(context *cli.Context, userClient *http.Client) (string, string) {
	xmppJID, authCode := initRobotAccount(context, userClient)
	token := verifyRobotAccount(authCode)

	return xmppJID, token
}

// createCloudConfig creates a config object that supports cloud and (optionally) local mode.
func createCloudConfig(context *cli.Context, xmppJID, robotRefreshToken, userRefreshToken, shareScope, proxyName string, localEnable bool) *lib.Config {
	return &lib.Config{
		XMPPJID:                   xmppJID,
		RobotRefreshToken:         robotRefreshToken,
		UserRefreshToken:          userRefreshToken,
		ShareScope:                shareScope,
		ProxyName:                 proxyName,
		XMPPServer:                lib.DefaultConfig.XMPPServer,
		XMPPPort:                  uint16(context.Int("xmpp-port")),
		XMPPPingTimeout:           context.String("gcp-xmpp-ping-timeout"),
		XMPPPingInterval:          context.String("gcp-xmpp-ping-interval-default"),
		GCPBaseURL:                lib.DefaultConfig.GCPBaseURL,
		GCPOAuthClientID:          lib.DefaultConfig.GCPOAuthClientID,
		GCPOAuthClientSecret:      lib.DefaultConfig.GCPOAuthClientSecret,
		GCPOAuthAuthURL:           lib.DefaultConfig.GCPOAuthAuthURL,
		GCPOAuthTokenURL:          lib.DefaultConfig.GCPOAuthTokenURL,
		GCPMaxConcurrentDownloads: uint(context.Int("gcp-max-concurrent-downloads")),

		CUPSMaxConnections:           uint(context.Int("cups-max-connections")),
		CUPSConnectTimeout:           context.String("cups-connect-timeout"),
		CUPSJobQueueSize:             uint(context.Int("cups-job-queue-size")),
		CUPSPrinterPollInterval:      context.String("cups-printer-poll-interval"),
		CUPSPrinterAttributes:        lib.DefaultConfig.CUPSPrinterAttributes,
		CUPSJobFullUsername:          context.Bool("cups-job-full-username"),
		CUPSIgnoreRawPrinters:        context.Bool("cups-ignore-raw-printers"),
		CopyPrinterInfoToDisplayName: context.Bool("copy-printer-info-to-display-name"),
		PrefixJobIDToJobTitle:        context.Bool("prefix-job-id-to-job-title"),
		DisplayNamePrefix:            context.String("display-name-prefix"),
		MonitorSocketFilename:        context.String("monitor-socket-filename"),
		SNMPEnable:                   context.Bool("snmp-enable"),
		SNMPCommunity:                context.String("snmp-community"),
		SNMPMaxConnections:           uint(context.Int("snmp-max-connections")),
		LocalPrintingEnable:          localEnable,
		CloudPrintingEnable:          true,
		LogFileName:                  context.String("log-file-name"),
		LogFileMaxMegabytes:          uint(context.Int("log-file-max-megabytes")),
		LogMaxFiles:                  uint(context.Int("log-max-files")),
		LogLevel:                     context.String("log-level"),
	}
}

// createLocalConfig creates a config object that supports local mode.
func createLocalConfig(context *cli.Context) *lib.Config {
	return &lib.Config{
		CUPSMaxConnections:           uint(context.Int("cups-max-connections")),
		CUPSConnectTimeout:           context.String("cups-connect-timeout"),
		CUPSJobQueueSize:             uint(context.Int("cups-job-queue-size")),
		CUPSPrinterPollInterval:      context.String("cups-printer-poll-interval"),
		CUPSPrinterAttributes:        lib.DefaultConfig.CUPSPrinterAttributes,
		CUPSJobFullUsername:          context.Bool("cups-job-full-username"),
		CUPSIgnoreRawPrinters:        context.Bool("cups-ignore-raw-printers"),
		CopyPrinterInfoToDisplayName: context.Bool("copy-printer-info-to-display-name"),
		PrefixJobIDToJobTitle:        context.Bool("prefix-job-id-to-job-title"),
		DisplayNamePrefix:            context.String("display-name-prefix"),
		MonitorSocketFilename:        context.String("monitor-socket-filename"),
		SNMPEnable:                   context.Bool("snmp-enable"),
		SNMPCommunity:                context.String("snmp-community"),
		SNMPMaxConnections:           uint(context.Int("snmp-max-connections")),
		LocalPrintingEnable:          true,
		CloudPrintingEnable:          false,
		LogFileName:                  context.String("log-file-name"),
		LogFileMaxMegabytes:          uint(context.Int("log-file-max-megabytes")),
		LogMaxFiles:                  uint(context.Int("log-max-files")),
		LogLevel:                     context.String("log-level"),
	}
}

func writeConfigFile(context *cli.Context, config *lib.Config) string {
	if configFilename, err := config.ToFile(context); err != nil {
		log.Fatalln(err)
	} else {
		return configFilename
	}
	panic("unreachable")
}

func scanNonEmptyString(prompt string) string {
	for {
		var answer string
		fmt.Println(prompt)
		if length, err := fmt.Scan(&answer); err != nil {
			log.Fatalln(err)
		} else if length > 0 {
			fmt.Println("")
			return answer
		}
	}
	panic("unreachable")
}

func scanYesOrNo(question string) bool {
	for {
		var answer string
		fmt.Println(question)
		if _, err := fmt.Scan(&answer); err != nil {
			log.Fatalln(err)
		} else if parsed, value := stringToBool(answer); parsed {
			fmt.Println("")
			return value
		}
	}
	panic("unreachable")
}

// The first return value is true if a boolean value could be parsed.
// The second return value is the parsed boolean value if the first return value is true.
func stringToBool(val string) (bool, bool) {
	if len(val) > 0 {
		switch strings.ToLower(val[0:1]) {
		case "y", "t", "1":
			return true, true
		case "n", "f", "0":
			return true, false
		default:
			return false, true
		}
	}
	return false, false
}

func initConfigFile(context *cli.Context) {
	var localEnable bool
	if context.IsSet("local-printing-enable") {
		localEnable = context.Bool("local-printing-enable")
	} else {
		fmt.Println("\"Local printing\" means that clients print directly to the connector via local subnet,")
		fmt.Println("and that an Internet connection is neither necessary nor used.")
		localEnable = scanYesOrNo("Enable local printing?")
	}

	var cloudEnable bool
	if context.IsSet("cloud-printing-enable") {
		cloudEnable = context.Bool("cloud-printing-enable")
	} else {
		fmt.Println("\"Cloud printing\" means that clients can print from anywhere on the Internet,")
		fmt.Println("and that printers must be explicitly shared with users.")
		cloudEnable = scanYesOrNo("Enable cloud printing?")
	}

	if !localEnable && !cloudEnable {
		log.Fatalln("Try again. Either local or cloud (or both) must be enabled for the connector to do something.")
	}

	var config *lib.Config

	var xmppJID, robotRefreshToken, userRefreshToken, shareScope, proxyName string
	if cloudEnable {
		if context.IsSet("share-scope") {
			shareScope = context.String("share-scope")
		} else if scanYesOrNo("Retain the user OAuth token to enable automatic sharing?") {
			shareScope = scanNonEmptyString("User or group email address to share with:")
		}

		if context.IsSet("proxy-name") {
			proxyName = context.String("proxy-name")
		} else {
			proxyName = scanNonEmptyString("Proxy name for this GCP CUPS Connector:")
		}

		var userClient *http.Client
		if context.IsSet("gcp-user-refresh-token") {
			userClient = getUserClientFromToken(context)
		} else {
			var urt string
			userClient, urt = getUserClientFromUser(context)
			if shareScope != "" {
				userRefreshToken = urt
			}
		}

		xmppJID, robotRefreshToken = createRobotAccount(context, userClient)

		fmt.Println("Acquired OAuth credentials for robot account")
		fmt.Println("")
		config = createCloudConfig(context, xmppJID, robotRefreshToken, userRefreshToken, shareScope, proxyName, localEnable)

	} else {
		config = createLocalConfig(context)
	}

	configFilename := writeConfigFile(context, config)
	fmt.Printf("The config file %s is ready to rock.\n", configFilename)
	if cloudEnable {
		fmt.Println("Keep it somewhere safe, as it contains an OAuth refresh token.")
	}

	socketDirectory := filepath.Dir(context.String("monitor-socket-filename"))
	if _, err := os.Stat(socketDirectory); os.IsNotExist(err) {
		fmt.Println("")
		fmt.Printf("When the connector runs, be sure the socket directory %s exists.\n", socketDirectory)
	}
}
