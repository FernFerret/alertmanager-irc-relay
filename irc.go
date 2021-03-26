// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/tls"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	irc "github.com/fluffle/goirc/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	pingFrequencySecs          = 60
	connectionTimeoutSecs      = 30
	nickservWaitSecs           = 10
	ircConnectMaxBackoffSecs   = 300
	ircConnectBackoffResetSecs = 1800
)

var (
	ircConnectedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "irc_connected",
		Help: "Whether the IRC connection is established",
	})
	ircSentMsgs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "irc_sent_msgs",
		Help: "Number of IRC messages sent"},
		[]string{"ircchannel"},
	)
	ircSendMsgErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "irc_send_msg_errors",
		Help: "Errors while sending IRC messages"},
		[]string{"ircchannel", "error"},
	)
)

func loggerHandler(_ *irc.Conn, line *irc.Line) {
	log.Printf("Received: '%s'", line.Raw)
}

type IRCNotifier struct {
	// Nick stores the nickname specified in the config, because irc.Client
	// might change its copy.
	Nick         string
	NickPassword string
	Client       *irc.Conn
	AlertMsgs    chan AlertMsg

	stopCtx context.Context
	stopWg  *sync.WaitGroup

	// irc.Conn has a Connected() method that can tell us wether the TCP
	// connection is up, and thus if we should trigger connect/disconnect.
	// We need to track the session establishment also at a higher level to
	// understand when the server has accepted us and thus when we can join
	// channels, send notices, etc.
	sessionUp         bool
	sessionUpSignal   chan bool
	sessionDownSignal chan bool

	channelReconciler *ChannelReconciler

	UsePrivmsg bool

	NickservDelayWait time.Duration
	BackoffCounter    Delayer
}

func NewIRCNotifier(stopCtx context.Context, stopWg *sync.WaitGroup, config *Config, alertMsgs chan AlertMsg, delayerMaker DelayerMaker) (*IRCNotifier, error) {

	ircConfig := irc.NewConfig(config.IRCNick)
	ircConfig.Me.Ident = config.IRCNick
	ircConfig.Me.Name = config.IRCRealName
	ircConfig.Server = strings.Join(
		[]string{config.IRCHost, strconv.Itoa(config.IRCPort)}, ":")
	ircConfig.Pass = config.IRCHostPass
	ircConfig.SSL = config.IRCUseSSL
	ircConfig.SSLConfig = &tls.Config{
		ServerName:         config.IRCHost,
		InsecureSkipVerify: !config.IRCVerifySSL,
	}
	ircConfig.PingFreq = pingFrequencySecs * time.Second
	ircConfig.Timeout = connectionTimeoutSecs * time.Second
	ircConfig.NewNick = func(n string) string { return n + "^" }

	client := irc.Client(ircConfig)

	backoffCounter := delayerMaker.NewDelayer(
		ircConnectMaxBackoffSecs, ircConnectBackoffResetSecs,
		time.Second)

	channelReconciler := NewChannelReconciler(config, client, delayerMaker)

	notifier := &IRCNotifier{
		Nick:              config.IRCNick,
		NickPassword:      config.IRCNickPass,
		Client:            client,
		AlertMsgs:         alertMsgs,
		stopCtx:           stopCtx,
		stopWg:            stopWg,
		sessionUpSignal:   make(chan bool),
		sessionDownSignal: make(chan bool),
		channelReconciler: channelReconciler,
		UsePrivmsg:        config.UsePrivmsg,
		NickservDelayWait: nickservWaitSecs * time.Second,
		BackoffCounter:    backoffCounter,
	}

	notifier.registerHandlers()

	return notifier, nil
}

func (n *IRCNotifier) registerHandlers() {
	n.Client.HandleFunc(irc.CONNECTED,
		func(*irc.Conn, *irc.Line) {
			log.Printf("Session established")
			n.sessionUpSignal <- true
		})

	n.Client.HandleFunc(irc.DISCONNECTED,
		func(*irc.Conn, *irc.Line) {
			log.Printf("Disconnected from IRC")
			n.sessionDownSignal <- false
		})

	for _, event := range []string{irc.NOTICE, "433"} {
		n.Client.HandleFunc(event, loggerHandler)
	}
}

func (n *IRCNotifier) MaybeIdentifyNick() {
	if n.NickPassword == "" {
		return
	}

	// Very lazy/optimistic, but this is good enough for my irssi config,
	// so it should work here as well.
	currentNick := n.Client.Me().Nick
	if currentNick != n.Nick {
		log.Printf("My nick is '%s', sending GHOST to NickServ to get '%s'",
			currentNick, n.Nick)
		n.Client.Privmsgf("NickServ", "GHOST %s %s", n.Nick,
			n.NickPassword)
		time.Sleep(n.NickservDelayWait)

		log.Printf("Changing nick to '%s'", n.Nick)
		n.Client.Nick(n.Nick)
	}
	log.Printf("Sending IDENTIFY to NickServ")
	n.Client.Privmsgf("NickServ", "IDENTIFY %s", n.NickPassword)
	time.Sleep(n.NickservDelayWait)
}

func (n *IRCNotifier) MaybeSendAlertMsg(alertMsg *AlertMsg) {
	if !n.sessionUp {
		log.Printf("Cannot send alert to %s : IRC not connected",
			alertMsg.Channel)
		ircSendMsgErrors.WithLabelValues(alertMsg.Channel, "not_connected").Inc()
		return
	}
	n.channelReconciler.JoinChannel(&IRCChannel{Name: alertMsg.Channel})

	if n.UsePrivmsg {
		n.Client.Privmsg(alertMsg.Channel, alertMsg.Alert)
	} else {
		n.Client.Notice(alertMsg.Channel, alertMsg.Alert)
	}
	ircSentMsgs.WithLabelValues(alertMsg.Channel).Inc()
}

func (n *IRCNotifier) ShutdownPhase() {
	if n.Client.Connected() {
		log.Printf("IRC client connected, quitting")
		n.Client.Quit("see ya")

		if n.sessionUp {
			log.Printf("Session is up, wait for IRC disconnect to complete")
			select {
			case <-n.sessionDownSignal:
			case <-time.After(n.Client.Config().Timeout):
				log.Printf("Timeout while waiting for IRC disconnect to complete, stopping anyway")
			}
		}
	}
}

func (n *IRCNotifier) ConnectedPhase() {
	select {
	case alertMsg := <-n.AlertMsgs:
		n.MaybeSendAlertMsg(&alertMsg)
	case <-n.sessionDownSignal:
		n.sessionUp = false
		n.channelReconciler.CleanupChannels()
		n.Client.Quit("see ya")
		ircConnectedGauge.Set(0)
	case <-n.stopCtx.Done():
		log.Printf("IRC routine asked to terminate")
	}
}

func (n *IRCNotifier) SetupPhase() {
	if !n.Client.Connected() {
		log.Printf("Connecting to IRC %s", n.Client.Config().Server)
		if ok := n.BackoffCounter.DelayContext(n.stopCtx); !ok {
			return
		}
		if err := n.Client.ConnectContext(n.stopCtx); err != nil {
			log.Printf("Could not connect to IRC: %s", err)
			return
		}
		log.Printf("Connected to IRC server, waiting to establish session")
	}
	select {
	case <-n.sessionUpSignal:
		n.sessionUp = true
		n.MaybeIdentifyNick()
		n.channelReconciler.JoinChannels()
		ircConnectedGauge.Set(1)
	case <-n.sessionDownSignal:
		log.Printf("Receiving a session down before the session is up, this is odd")
	case <-n.stopCtx.Done():
		log.Printf("IRC routine asked to terminate")
	}
}

func (n *IRCNotifier) Run() {
	defer n.stopWg.Done()

	for n.stopCtx.Err() != context.Canceled {
		if !n.sessionUp {
			n.SetupPhase()
		} else {
			n.ConnectedPhase()
		}
	}
	n.ShutdownPhase()
}
