// Copyright (c) 2012-2014 Jeremy Latt
// Copyright (c) 2014-2015 Edmund Huber
// Copyright (c) 2016-2017 Daniel Oaks <daniel@danieloaks.net>
// released under the MIT license

package irc

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/goshuirc/irc-go/ircfmt"

	"github.com/oragono/oragono/irc/caps"
	"github.com/oragono/oragono/irc/connection_limits"
	"github.com/oragono/oragono/irc/history"
	"github.com/oragono/oragono/irc/logger"
	"github.com/oragono/oragono/irc/modes"
	"github.com/oragono/oragono/irc/mysql"
	"github.com/oragono/oragono/irc/sno"
	"github.com/oragono/oragono/irc/utils"
	"github.com/tidwall/buntdb"
)

var (
	// common error line to sub values into
	errorMsg = "ERROR :%s\r\n"

	// three final parameters of 004 RPL_MYINFO, enumerating our supported modes
	rplMyInfo1, rplMyInfo2, rplMyInfo3 = modes.RplMyInfo()

	// whitelist of caps to serve on the STS-only listener. In particular,
	// never advertise SASL, to discourage people from sending their passwords:
	stsOnlyCaps = caps.NewSet(caps.STS, caps.MessageTags, caps.ServerTime, caps.Batch, caps.LabeledResponse, caps.EchoMessage, caps.Nope)

	// we only have standard channels for now. TODO: any updates to this
	// will also need to be reflected in CasefoldChannel
	chanTypes = "#"

	throttleMessage = "You have attempted to connect too many times within a short duration. Wait a while, and you will be able to connect."
)

// Server is the main Oragono server.
type Server struct {
	accounts          AccountManager
	channels          ChannelManager
	channelRegistry   ChannelRegistry
	clients           ClientManager
	config            unsafe.Pointer
	configFilename    string
	connectionLimiter connection_limits.Limiter
	ctime             time.Time
	dlines            *DLineManager
	helpIndexManager  HelpIndexManager
	klines            *KLineManager
	listeners         map[string]IRCListener
	logger            *logger.Manager
	monitorManager    MonitorManager
	name              string
	nameCasefolded    string
	rehashMutex       sync.Mutex // tier 4
	rehashSignal      chan os.Signal
	pprofServer       *http.Server
	resumeManager     ResumeManager
	signals           chan os.Signal
	snomasks          SnoManager
	store             *buntdb.DB
	historyDB         mysql.MySQL
	torLimiter        connection_limits.TorLimiter
	whoWas            WhoWasList
	stats             Stats
	semaphores        ServerSemaphores
}

// NewServer returns a new Oragono server.
func NewServer(config *Config, logger *logger.Manager) (*Server, error) {
	// initialize data structures
	server := &Server{
		ctime:        time.Now().UTC(),
		listeners:    make(map[string]IRCListener),
		logger:       logger,
		rehashSignal: make(chan os.Signal, 1),
		signals:      make(chan os.Signal, len(ServerExitSignals)),
	}

	server.clients.Initialize()
	server.semaphores.Initialize()
	server.resumeManager.Initialize(server)
	server.whoWas.Initialize(config.Limits.WhowasEntries)
	server.monitorManager.Initialize()
	server.snomasks.Initialize()

	if err := server.applyConfig(config); err != nil {
		return nil, err
	}

	// Attempt to clean up when receiving these signals.
	signal.Notify(server.signals, ServerExitSignals...)
	signal.Notify(server.rehashSignal, syscall.SIGHUP)

	return server, nil
}

// Shutdown shuts down the server.
func (server *Server) Shutdown() {
	//TODO(dan): Make sure we disallow new nicks
	for _, client := range server.clients.AllClients() {
		client.Notice("Server is shutting down")
		if client.AlwaysOn() {
			client.Store(IncludeLastSeen)
		}
	}

	if err := server.store.Close(); err != nil {
		server.logger.Error("shutdown", fmt.Sprintln("Could not close datastore:", err))
	}

	server.historyDB.Close()
}

// Run starts the server.
func (server *Server) Run() {
	// defer closing db/store
	defer server.store.Close()

	for {
		select {
		case <-server.signals:
			server.Shutdown()
			return

		case <-server.rehashSignal:
			go func() {
				server.logger.Info("server", "Rehashing due to SIGHUP")
				server.rehash()
			}()
		}
	}
}

func (server *Server) checkBans(ipaddr net.IP) (banned bool, message string) {
	// check DLINEs
	isBanned, info := server.dlines.CheckIP(ipaddr)
	if isBanned {
		server.logger.Info("connect-ip", fmt.Sprintf("Client from %v rejected by d-line", ipaddr))
		return true, info.BanMessage("You are banned from this server (%s)")
	}

	// check connection limits
	err := server.connectionLimiter.AddClient(ipaddr)
	if err == connection_limits.ErrLimitExceeded {
		// too many connections from one client, tell the client and close the connection
		server.logger.Info("connect-ip", fmt.Sprintf("Client from %v rejected for connection limit", ipaddr))
		return true, "Too many clients from your network"
	} else if err == connection_limits.ErrThrottleExceeded {
		duration := server.Config().Server.IPLimits.BanDuration
		if duration == 0 {
			return false, ""
		}
		server.dlines.AddIP(ipaddr, duration, throttleMessage, "Exceeded automated connection throttle", "auto.connection.throttler")
		// they're DLINE'd for 15 minutes or whatever, so we can reset the connection throttle now,
		// and once their temporary DLINE is finished they can fill up the throttler again
		server.connectionLimiter.ResetThrottle(ipaddr)

		// this might not show up properly on some clients, but our objective here is just to close it out before it has a load impact on us
		server.logger.Info(
			"connect-ip",
			fmt.Sprintf("Client from %v exceeded connection throttle, d-lining for %v", ipaddr, duration))
		return true, throttleMessage
	} else if err != nil {
		server.logger.Warning("internal", "unexpected ban result", err.Error())
	}

	return false, ""
}

func (server *Server) checkTorLimits() (banned bool, message string) {
	switch server.torLimiter.AddClient() {
	case connection_limits.ErrLimitExceeded:
		return true, "Too many clients from the Tor network"
	case connection_limits.ErrThrottleExceeded:
		return true, "Exceeded connection throttle for the Tor network"
	default:
		return false, ""
	}
}

//
// server functionality
//

func (server *Server) tryRegister(c *Client, session *Session) (exiting bool) {
	// if the session just sent us a RESUME line, try to resume
	if session.resumeDetails != nil {
		session.tryResume()
		return // whether we succeeded or failed, either way `c` is not getting registered
	}

	// try to complete registration normally
	// XXX(#1057) username can be filled in by an ident query without the client
	// having sent USER: check for both username and realname to ensure they did
	if c.preregNick == "" || c.username == "" || c.realname == "" || session.capState == caps.NegotiatingState {
		return
	}

	if c.isSTSOnly {
		server.playSTSBurst(session)
		return true
	}

	// client MUST send PASS if necessary, or authenticate with SASL if necessary,
	// before completing the other registration commands
	authOutcome := c.isAuthorized(server.Config(), session)
	var quitMessage string
	switch authOutcome {
	case authFailPass:
		quitMessage = c.t("Password incorrect")
		c.Send(nil, server.name, ERR_PASSWDMISMATCH, "*", quitMessage)
	case authFailSaslRequired, authFailTorSaslRequired:
		quitMessage = c.t("You must log in with SASL to join this server")
		c.Send(nil, c.server.name, "FAIL", "*", "ACCOUNT_REQUIRED", quitMessage)
	}
	if authOutcome != authSuccess {
		c.Quit(quitMessage, nil)
		return true
	}

	// we have the final value of the IP address: do the hostname lookup
	// (nickmask will be set below once nickname assignment succeeds)
	if session.rawHostname == "" {
		session.client.lookupHostname(session, false)
	}

	rb := NewResponseBuffer(session)
	nickError := performNickChange(server, c, c, session, c.preregNick, rb)
	rb.Send(true)
	if nickError == errInsecureReattach {
		c.Quit(c.t("You can't mix secure and insecure connections to this account"), nil)
		return true
	} else if nickError != nil {
		c.preregNick = ""
		return false
	}

	if session.client != c {
		// reattached, bail out.
		// we'll play the reg burst later, on the new goroutine associated with
		// (thisSession, otherClient). This is to avoid having to transfer state
		// like nickname, hostname, etc. to show the correct values in the reg burst.
		return false
	}

	// check KLINEs
	isBanned, info := server.klines.CheckMasks(c.AllNickmasks()...)
	if isBanned {
		c.Quit(info.BanMessage(c.t("You are banned from this server (%s)")), nil)
		return true
	}

	// Apply default user modes (without updating the invisible counter)
	// The number of invisible users will be updated by server.stats.Register
	// if we're using default user mode +i.
	for _, defaultMode := range server.Config().Accounts.defaultUserModes {
		c.SetMode(defaultMode, true)
	}

	// registration has succeeded:
	c.SetRegistered()

	// count new user in statistics
	server.stats.Register(c.HasMode(modes.Invisible))
	server.monitorManager.AlertAbout(c.Nick(), c.NickCasefolded(), true)

	server.playRegistrationBurst(session)
	return false
}

func (server *Server) playSTSBurst(session *Session) {
	nick := utils.SafeErrorParam(session.client.preregNick)
	session.Send(nil, server.name, RPL_WELCOME, nick, fmt.Sprintf("Welcome to the Internet Relay Network %s", nick))
	session.Send(nil, server.name, RPL_YOURHOST, nick, fmt.Sprintf("Your host is %[1]s, running version %[2]s", server.name, "oragono"))
	session.Send(nil, server.name, RPL_CREATED, nick, fmt.Sprintf("This server was created %s", time.Time{}.Format(time.RFC1123)))
	session.Send(nil, server.name, RPL_MYINFO, nick, server.name, "oragono", "o", "o", "o")
	session.Send(nil, server.name, RPL_ISUPPORT, nick, "CASEMAPPING=ascii", "are supported by this server")
	session.Send(nil, server.name, ERR_NOMOTD, nick, "MOTD is unavailable")
	for _, line := range server.Config().Server.STS.bannerLines {
		session.Send(nil, server.name, "NOTICE", nick, line)
	}
}

func (server *Server) playRegistrationBurst(session *Session) {
	c := session.client
	// continue registration
	d := c.Details()
	server.logger.Info("connect", fmt.Sprintf("Client connected [%s] [u:%s] [r:%s]", d.nick, d.username, d.realname))
	server.snomasks.Send(sno.LocalConnects, fmt.Sprintf("Client connected [%s] [u:%s] [h:%s] [ip:%s] [r:%s]", d.nick, d.username, session.rawHostname, session.IP().String(), d.realname))

	// send welcome text
	//NOTE(dan): we specifically use the NICK here instead of the nickmask
	// see http://modern.ircdocs.horse/#rplwelcome-001 for details on why we avoid using the nickmask
	session.Send(nil, server.name, RPL_WELCOME, d.nick, fmt.Sprintf(c.t("Welcome to the Internet Relay Network %s"), d.nick))
	session.Send(nil, server.name, RPL_YOURHOST, d.nick, fmt.Sprintf(c.t("Your host is %[1]s, running version %[2]s"), server.name, Ver))
	session.Send(nil, server.name, RPL_CREATED, d.nick, fmt.Sprintf(c.t("This server was created %s"), server.ctime.Format(time.RFC1123)))
	session.Send(nil, server.name, RPL_MYINFO, d.nick, server.name, Ver, rplMyInfo1, rplMyInfo2, rplMyInfo3)

	rb := NewResponseBuffer(session)
	server.RplISupport(c, rb)
	server.Lusers(c, rb)
	server.MOTD(c, rb)
	rb.Send(true)

	modestring := c.ModeString()
	if modestring != "+" {
		session.Send(nil, server.name, RPL_UMODEIS, d.nick, modestring)
	}

	c.attemptAutoOper(session)

	if server.logger.IsLoggingRawIO() {
		session.Send(nil, c.server.name, "NOTICE", d.nick, c.t("This server is in debug mode and is logging all user I/O. If you do not wish for everything you send to be readable by the server owner(s), please disconnect."))
	}
}

// RplISupport outputs our ISUPPORT lines to the client. This is used on connection and in VERSION responses.
func (server *Server) RplISupport(client *Client, rb *ResponseBuffer) {
	translatedISupport := client.t("are supported by this server")
	nick := client.Nick()
	config := server.Config()
	for _, cachedTokenLine := range config.Server.isupport.CachedReply {
		length := len(cachedTokenLine) + 2
		tokenline := make([]string, length)
		tokenline[0] = nick
		copy(tokenline[1:], cachedTokenLine)
		tokenline[length-1] = translatedISupport
		rb.Add(nil, server.name, RPL_ISUPPORT, tokenline...)
	}
}

func (server *Server) Lusers(client *Client, rb *ResponseBuffer) {
	nick := client.Nick()
	stats := server.stats.GetValues()

	rb.Add(nil, server.name, RPL_LUSERCLIENT, nick, fmt.Sprintf(client.t("There are %[1]d users and %[2]d invisible on %[3]d server(s)"), stats.Total-stats.Invisible, stats.Invisible, 1))
	rb.Add(nil, server.name, RPL_LUSEROP, nick, strconv.Itoa(stats.Operators), client.t("IRC Operators online"))
	rb.Add(nil, server.name, RPL_LUSERUNKNOWN, nick, strconv.Itoa(stats.Unknown), client.t("unregistered connections"))
	rb.Add(nil, server.name, RPL_LUSERCHANNELS, nick, strconv.Itoa(server.channels.Len()), client.t("channels formed"))
	rb.Add(nil, server.name, RPL_LUSERME, nick, fmt.Sprintf(client.t("I have %[1]d clients and %[2]d servers"), stats.Total, 0))
	total := strconv.Itoa(stats.Total)
	max := strconv.Itoa(stats.Max)
	rb.Add(nil, server.name, RPL_LOCALUSERS, nick, total, max, fmt.Sprintf(client.t("Current local users %[1]s, max %[2]s"), total, max))
	rb.Add(nil, server.name, RPL_GLOBALUSERS, nick, total, max, fmt.Sprintf(client.t("Current global users %[1]s, max %[2]s"), total, max))
}

// MOTD serves the Message of the Day.
func (server *Server) MOTD(client *Client, rb *ResponseBuffer) {
	motdLines := server.Config().Server.motdLines

	if len(motdLines) < 1 {
		rb.Add(nil, server.name, ERR_NOMOTD, client.nick, client.t("MOTD File is missing"))
		return
	}

	rb.Add(nil, server.name, RPL_MOTDSTART, client.nick, fmt.Sprintf(client.t("- %s Message of the day - "), server.name))
	for _, line := range motdLines {
		rb.Add(nil, server.name, RPL_MOTD, client.nick, line)
	}
	rb.Add(nil, server.name, RPL_ENDOFMOTD, client.nick, client.t("End of MOTD command"))
}

// WhoisChannelsNames returns the common channel names between two users.
func (client *Client) WhoisChannelsNames(target *Client, multiPrefix bool) []string {
	var chstrs []string
	for _, channel := range target.Channels() {
		// channel is secret and the target can't see it
		if !client.HasMode(modes.Operator) {
			if (target.HasMode(modes.Invisible) || channel.flags.HasMode(modes.Secret)) && !channel.hasClient(client) {
				continue
			}
		}
		chstrs = append(chstrs, channel.ClientPrefixes(target, multiPrefix)+channel.name)
	}
	return chstrs
}

func (client *Client) getWhoisOf(target *Client, rb *ResponseBuffer) {
	cnick := client.Nick()
	targetInfo := target.Details()
	rb.Add(nil, client.server.name, RPL_WHOISUSER, cnick, targetInfo.nick, targetInfo.username, targetInfo.hostname, "*", targetInfo.realname)
	tnick := targetInfo.nick

	whoischannels := client.WhoisChannelsNames(target, rb.session.capabilities.Has(caps.MultiPrefix))
	if whoischannels != nil {
		rb.Add(nil, client.server.name, RPL_WHOISCHANNELS, cnick, tnick, strings.Join(whoischannels, " "))
	}
	tOper := target.Oper()
	if tOper != nil {
		rb.Add(nil, client.server.name, RPL_WHOISOPERATOR, cnick, tnick, tOper.WhoisLine)
	}
	if client.HasMode(modes.Operator) || client == target {
		rb.Add(nil, client.server.name, RPL_WHOISACTUALLY, cnick, tnick, fmt.Sprintf("%s@%s", targetInfo.username, target.RawHostname()), target.IPString(), client.t("Actual user@host, Actual IP"))
	}
	if target.HasMode(modes.TLS) {
		rb.Add(nil, client.server.name, RPL_WHOISSECURE, cnick, tnick, client.t("is using a secure connection"))
	}
	if targetInfo.accountName != "*" {
		rb.Add(nil, client.server.name, RPL_WHOISACCOUNT, cnick, tnick, targetInfo.accountName, client.t("is logged in as"))
	}
	if target.HasMode(modes.Bot) {
		rb.Add(nil, client.server.name, RPL_WHOISBOT, cnick, tnick, ircfmt.Unescape(fmt.Sprintf(client.t("is a $bBot$b on %s"), client.server.Config().Network.Name)))
	}

	if client == target || client.HasMode(modes.Operator) {
		for _, session := range target.Sessions() {
			if session.certfp != "" {
				rb.Add(nil, client.server.name, RPL_WHOISCERTFP, cnick, tnick, fmt.Sprintf(client.t("has client certificate fingerprint %s"), session.certfp))
			}
		}
	}
	rb.Add(nil, client.server.name, RPL_WHOISIDLE, cnick, tnick, strconv.FormatUint(target.IdleSeconds(), 10), strconv.FormatInt(target.SignonTime(), 10), client.t("seconds idle, signon time"))
	if target.Away() {
		rb.Add(nil, client.server.name, RPL_AWAY, cnick, tnick, target.AwayMessage())
	}
}

const WhoFieldMinimum = int('a') // lowest rune value
const WhoFieldMaximum = int('z')

type WhoFields [WhoFieldMaximum - WhoFieldMinimum + 1]bool

func (fields *WhoFields) Set(field rune) bool {
	index := int(field)
	if WhoFieldMinimum <= index && index <= WhoFieldMaximum {
		fields[int(field)-WhoFieldMinimum] = true
		return true
	} else {
		return false
	}
}
func (fields *WhoFields) Has(field rune) bool {
	return fields[int(field)-WhoFieldMinimum]
}

// rplWhoReply returns the WHO(X) reply between one user and another channel/user.
// who format:
// <channel> <user> <host> <server> <nick> <H|G>[*][~|&|@|%|+][B] :<hopcount> <real name>
// whox format:
// <type> <channel> <user> <ip> <host> <server> <nick> <H|G>[*][~|&|@|%|+][B] <hops> <idle> <account> <rank> :<real name>
func (client *Client) rplWhoReply(channel *Channel, target *Client, rb *ResponseBuffer, isWhox bool, fields WhoFields, whoType string) {
	params := []string{client.Nick()}

	details := target.Details()

	if fields.Has('t') {
		params = append(params, whoType)
	}
	if fields.Has('c') {
		fChannel := "*"
		if channel != nil {
			fChannel = channel.name
		}
		params = append(params, fChannel)
	}
	if fields.Has('u') {
		params = append(params, details.username)
	}
	if fields.Has('i') {
		fIP := "255.255.255.255"
		if client.HasMode(modes.Operator) || client == target {
			// you can only see a target's IP if they're you or you're an oper
			fIP = target.IPString()
		}
		params = append(params, fIP)
	}
	if fields.Has('h') {
		params = append(params, details.hostname)
	}
	if fields.Has('s') {
		params = append(params, target.server.name)
	}
	if fields.Has('n') {
		params = append(params, details.nick)
	}
	if fields.Has('f') { // "flags" (away + oper state + channel status prefix + bot)
		var flags strings.Builder
		if target.Away() {
			flags.WriteRune('G') // Gone
		} else {
			flags.WriteRune('H') // Here
		}

		if target.HasMode(modes.Operator) {
			flags.WriteRune('*')
		}

		if channel != nil {
			flags.WriteString(channel.ClientPrefixes(target, false))
		}

		if target.HasMode(modes.Bot) {
			flags.WriteRune('B')
		}

		params = append(params, flags.String())

	}
	if fields.Has('d') { // server hops from us to target
		params = append(params, "0")
	}
	if fields.Has('l') {
		params = append(params, fmt.Sprintf("%d", target.IdleSeconds()))
	}
	if fields.Has('a') {
		fAccount := "0"
		if target.accountName != "*" {
			// WHOX uses "0" to mean "no account"
			fAccount = target.accountName
		}
		params = append(params, fAccount)
	}
	if fields.Has('o') { // target's channel power level
		//TODO: implement this
		params = append(params, "0")
	}
	if fields.Has('r') {
		params = append(params, details.realname)
	}

	numeric := RPL_WHOSPCRPL
	if !isWhox {
		numeric = RPL_WHOREPLY
		// if this isn't WHOX, stick hops + realname at the end
		params = append(params, "0 "+details.realname)
	}

	rb.Add(nil, client.server.name, numeric, params...)
}

// rehash reloads the config and applies the changes from the config file.
func (server *Server) rehash() error {
	server.logger.Info("server", "Attempting rehash")

	// only let one REHASH go on at a time
	server.rehashMutex.Lock()
	defer server.rehashMutex.Unlock()

	server.logger.Debug("server", "Got rehash lock")

	config, err := LoadConfig(server.configFilename)
	if err != nil {
		server.logger.Error("server", "failed to load config file", err.Error())
		return err
	}

	err = server.applyConfig(config)
	if err != nil {
		server.logger.Error("server", "Failed to rehash", err.Error())
		return err
	}

	server.logger.Info("server", "Rehash completed successfully")
	return nil
}

func (server *Server) applyConfig(config *Config) (err error) {
	oldConfig := server.Config()
	initial := oldConfig == nil

	if initial {
		server.configFilename = config.Filename
		server.name = config.Server.Name
		server.nameCasefolded = config.Server.nameCasefolded
		globalCasemappingSetting = config.Server.Casemapping
		globalUtf8EnforcementSetting = config.Server.EnforceUtf8
	} else {
		// enforce configs that can't be changed after launch:
		if server.name != config.Server.Name {
			return fmt.Errorf("Server name cannot be changed after launching the server, rehash aborted")
		} else if oldConfig.Datastore.Path != config.Datastore.Path {
			return fmt.Errorf("Datastore path cannot be changed after launching the server, rehash aborted")
		} else if globalCasemappingSetting != config.Server.Casemapping {
			return fmt.Errorf("Casemapping cannot be changed after launching the server, rehash aborted")
		} else if globalUtf8EnforcementSetting != config.Server.EnforceUtf8 {
			return fmt.Errorf("UTF-8 enforcement cannot be changed after launching the server, rehash aborted")
		} else if oldConfig.Accounts.Multiclient.AlwaysOn != config.Accounts.Multiclient.AlwaysOn {
			return fmt.Errorf("Default always-on setting cannot be changed after launching the server, rehash aborted")
		}
	}

	server.logger.Info("server", "Using config file", server.configFilename)

	// first, reload config sections for functionality implemented in subpackages:
	wasLoggingRawIO := !initial && server.logger.IsLoggingRawIO()
	err = server.logger.ApplyConfig(config.Logging)
	if err != nil {
		return err
	}
	nowLoggingRawIO := server.logger.IsLoggingRawIO()
	// notify existing clients if raw i/o logging was enabled by a rehash
	sendRawOutputNotice := !wasLoggingRawIO && nowLoggingRawIO

	server.connectionLimiter.ApplyConfig(&config.Server.IPLimits)

	tlConf := &config.Server.TorListeners
	server.torLimiter.Configure(tlConf.MaxConnections, tlConf.ThrottleDuration, tlConf.MaxConnectionsPerDuration)

	// Translations
	server.logger.Debug("server", "Regenerating HELP indexes for new languages")
	server.helpIndexManager.GenerateIndices(config.languageManager)

	if oldConfig != nil {
		// if certain features were enabled by rehash, we need to load the corresponding data
		// from the store
		if !oldConfig.Accounts.NickReservation.Enabled {
			server.accounts.buildNickToAccountIndex(config)
		}
		if !oldConfig.Accounts.VHosts.Enabled {
			server.accounts.initVHostRequestQueue(config)
		}
		if !oldConfig.Channels.Registration.Enabled {
			server.channels.loadRegisteredChannels(config)
		}
		// resize history buffers as needed
		if oldConfig.History != config.History {
			for _, channel := range server.channels.Channels() {
				channel.resizeHistory(config)
			}
			for _, client := range server.clients.AllClients() {
				client.resizeHistory(config)
			}
		}
		if oldConfig.Accounts.Registration.Throttling != config.Accounts.Registration.Throttling {
			server.accounts.resetRegisterThrottle(config)
		}
	}

	server.logger.Info("server", "Using datastore", config.Datastore.Path)
	if initial {
		if err := server.loadDatastore(config); err != nil {
			return err
		}
	} else {
		if config.Datastore.MySQL.Enabled && config.Datastore.MySQL != oldConfig.Datastore.MySQL {
			server.historyDB.SetConfig(config.Datastore.MySQL)
		}
	}

	// now that the datastore is initialized, we can load the cloak secret from it
	// XXX this modifies config after the initial load, which is naughty,
	// but there's no data race because we haven't done SetConfig yet
	if config.Server.Cloaks.Enabled {
		config.Server.Cloaks.SetSecret(LoadCloakSecret(server.store))
	}

	// activate the new config
	server.SetConfig(config)

	// load [dk]-lines, registered users and channels, etc.
	if initial {
		if err := server.loadFromDatastore(config); err != nil {
			return err
		}
	}

	// burst new and removed caps
	addedCaps, removedCaps := config.Diff(oldConfig)
	var capBurstSessions []*Session
	added := make(map[caps.Version][]string)
	var removed []string

	if !addedCaps.Empty() || !removedCaps.Empty() {
		capBurstSessions = server.clients.AllWithCapsNotify()

		added[caps.Cap301] = addedCaps.Strings(caps.Cap301, config.Server.capValues, 0)
		added[caps.Cap302] = addedCaps.Strings(caps.Cap302, config.Server.capValues, 0)
		// removed never has values, so we leave it as Cap301
		removed = removedCaps.Strings(caps.Cap301, config.Server.capValues, 0)
	}

	for _, sSession := range capBurstSessions {
		// DEL caps and then send NEW ones so that updated caps get removed/added correctly
		if !removedCaps.Empty() {
			for _, capStr := range removed {
				sSession.Send(nil, server.name, "CAP", sSession.client.Nick(), "DEL", capStr)
			}
		}
		if !addedCaps.Empty() {
			for _, capStr := range added[sSession.capVersion] {
				sSession.Send(nil, server.name, "CAP", sSession.client.Nick(), "NEW", capStr)
			}
		}
	}

	server.setupPprofListener(config)

	// set RPL_ISUPPORT
	var newISupportReplies [][]string
	if oldConfig != nil {
		newISupportReplies = oldConfig.Server.isupport.GetDifference(&config.Server.isupport)
	}

	if len(config.Server.ProxyAllowedFrom) != 0 {
		server.logger.Info("server", "Proxied IPs will be accepted from", strings.Join(config.Server.ProxyAllowedFrom, ", "))
	}

	// we are now open for business
	err = server.setupListeners(config)

	if !initial {
		// push new info to all of our clients
		for _, sClient := range server.clients.AllClients() {
			for _, tokenline := range newISupportReplies {
				sClient.Send(nil, server.name, RPL_ISUPPORT, append([]string{sClient.nick}, tokenline...)...)
			}

			if sendRawOutputNotice {
				sClient.Notice(sClient.t("This server is in debug mode and is logging all user I/O. If you do not wish for everything you send to be readable by the server owner(s), please disconnect."))
			}
		}
	}

	return err
}

func (server *Server) setupPprofListener(config *Config) {
	pprofListener := ""
	if config.Debug.PprofListener != nil {
		pprofListener = *config.Debug.PprofListener
	}
	if server.pprofServer != nil {
		if pprofListener == "" || (pprofListener != server.pprofServer.Addr) {
			server.logger.Info("server", "Stopping pprof listener", server.pprofServer.Addr)
			server.pprofServer.Close()
			server.pprofServer = nil
		}
	}
	if pprofListener != "" && server.pprofServer == nil {
		ps := http.Server{
			Addr: pprofListener,
		}
		go func() {
			if err := ps.ListenAndServe(); err != nil {
				server.logger.Error("server", "pprof listener failed", err.Error())
			}
		}()
		server.pprofServer = &ps
		server.logger.Info("server", "Started pprof listener", server.pprofServer.Addr)
	}
}

func (server *Server) loadDatastore(config *Config) error {
	// open the datastore and load server state for which it (rather than config)
	// is the source of truth

	_, err := os.Stat(config.Datastore.Path)
	if os.IsNotExist(err) {
		server.logger.Warning("server", "database does not exist, creating it", config.Datastore.Path)
		err = initializeDB(config.Datastore.Path)
		if err != nil {
			return err
		}
	}

	db, err := OpenDatabase(config)
	if err == nil {
		server.store = db
		return nil
	} else {
		return fmt.Errorf("Failed to open datastore: %s", err.Error())
	}
}

func (server *Server) loadFromDatastore(config *Config) (err error) {
	// load *lines (from the datastores)
	server.logger.Debug("server", "Loading D/Klines")
	server.loadDLines()
	server.loadKLines()

	server.channelRegistry.Initialize(server)
	server.channels.Initialize(server)
	server.accounts.Initialize(server)

	if config.Datastore.MySQL.Enabled {
		server.historyDB.Initialize(server.logger, config.Datastore.MySQL)
		err = server.historyDB.Open()
		if err != nil {
			server.logger.Error("internal", "could not connect to mysql", err.Error())
			return err
		}
	}

	return nil
}

func (server *Server) setupListeners(config *Config) (err error) {
	logListener := func(addr string, config utils.ListenerConfig) {
		server.logger.Info("listeners",
			fmt.Sprintf("now listening on %s, tls=%t, tlsproxy=%t, tor=%t, websocket=%t.", addr, (config.TLSConfig != nil), config.RequireProxy, config.Tor, config.WebSocket),
		)
	}

	// update or destroy all existing listeners
	for addr := range server.listeners {
		currentListener := server.listeners[addr]
		newConfig, stillConfigured := config.Server.trueListeners[addr]

		if stillConfigured {
			if reloadErr := currentListener.Reload(newConfig); reloadErr == nil {
				logListener(addr, newConfig)
			} else {
				// stop the listener; we will attempt to replace it below
				currentListener.Stop()
				delete(server.listeners, addr)
			}
		} else {
			currentListener.Stop()
			delete(server.listeners, addr)
			server.logger.Info("listeners", fmt.Sprintf("stopped listening on %s.", addr))
		}
	}

	publicPlaintextListener := ""
	// create new listeners that were not previously configured,
	// or that couldn't be reloaded above:
	for newAddr, newConfig := range config.Server.trueListeners {
		if strings.HasPrefix(newAddr, ":") && !newConfig.Tor && !newConfig.STSOnly && newConfig.TLSConfig == nil {
			publicPlaintextListener = newAddr
		}
		_, exists := server.listeners[newAddr]
		if !exists {
			// make a new listener
			newListener, newErr := NewListener(server, newAddr, newConfig, config.Server.UnixBindMode)
			if newErr == nil {
				server.listeners[newAddr] = newListener
				logListener(newAddr, newConfig)
			} else {
				server.logger.Error("server", "couldn't listen on", newAddr, newErr.Error())
				err = newErr
			}
		}
	}

	if publicPlaintextListener != "" {
		server.logger.Warning("listeners", fmt.Sprintf("Your server is configured with public plaintext listener %s. Consider disabling it for improved security and privacy.", publicPlaintextListener))
	}

	return
}

// Gets the abstract sequence from which we're going to query history;
// we may already know the channel we're querying, or we may have
// to look it up via a string query. This function is responsible for
// privilege checking.
func (server *Server) GetHistorySequence(providedChannel *Channel, client *Client, query string) (channel *Channel, sequence history.Sequence, err error) {
	config := server.Config()
	// 4 cases: {persistent, ephemeral} x {normal, conversation}
	// with ephemeral history, target is implicit in the choice of `hist`,
	// and correspondent is "" if we're retrieving a channel or *, and the correspondent's name
	// if we're retrieving a DM conversation ("query buffer"). with persistent history,
	// target is always nonempty, and correspondent is either empty or nonempty as before.
	var status HistoryStatus
	var target, correspondent string
	var hist *history.Buffer
	channel = providedChannel
	if channel == nil {
		if strings.HasPrefix(query, "#") {
			channel = server.channels.Get(query)
			if channel == nil {
				return
			}
		}
	}
	if channel != nil {
		if !channel.hasClient(client) {
			err = errInsufficientPrivs
			return
		}
		status, target = channel.historyStatus(config)
		switch status {
		case HistoryEphemeral:
			hist = &channel.history
		case HistoryPersistent:
			// already set `target`
		default:
			return
		}
	} else {
		status, target = client.historyStatus(config)
		if query != "*" {
			correspondent, err = CasefoldName(query)
			if err != nil {
				return
			}
		}
		switch status {
		case HistoryEphemeral:
			hist = &client.history
		case HistoryPersistent:
			// already set `target`, and `correspondent` if necessary
		default:
			return
		}
	}

	var cutoff time.Time
	if config.History.Restrictions.ExpireTime != 0 {
		cutoff = time.Now().UTC().Add(-time.Duration(config.History.Restrictions.ExpireTime))
	}
	// #836: registration date cutoff is always enforced for DMs
	if config.History.Restrictions.EnforceRegistrationDate || channel == nil {
		regCutoff := client.historyCutoff()
		// take the later of the two cutoffs
		if regCutoff.After(cutoff) {
			cutoff = regCutoff
		}
	}
	// #836 again: grace period is never applied to DMs
	if !cutoff.IsZero() && channel != nil {
		cutoff = cutoff.Add(-time.Duration(config.History.Restrictions.GracePeriod))
	}

	if hist != nil {
		sequence = hist.MakeSequence(correspondent, cutoff)
	} else if target != "" {
		sequence = server.historyDB.MakeSequence(target, correspondent, cutoff)
	}
	return
}

func (server *Server) ForgetHistory(accountName string) {
	// sanity check
	if accountName == "*" {
		return
	}

	config := server.Config()
	if !config.History.Enabled {
		return
	}

	if cfAccount, err := CasefoldName(accountName); err == nil {
		server.historyDB.Forget(cfAccount)
	}

	persistent := config.History.Persistent
	if persistent.Enabled && persistent.UnregisteredChannels && persistent.RegisteredChannels == PersistentMandatory && persistent.DirectMessages == PersistentMandatory {
		return
	}

	predicate := func(item *history.Item) bool { return item.AccountName == accountName }

	for _, channel := range server.channels.Channels() {
		channel.history.Delete(predicate)
	}

	for _, client := range server.clients.AllClients() {
		client.history.Delete(predicate)
	}
}

// deletes a message. target is a hint about what buffer it's in (not required for
// persistent history, where all the msgids are indexed together). if accountName
// is anything other than "*", it must match the recorded AccountName of the message
func (server *Server) DeleteMessage(target, msgid, accountName string) (err error) {
	config := server.Config()
	var hist *history.Buffer

	if target != "" {
		if target[0] == '#' {
			channel := server.channels.Get(target)
			if channel != nil {
				if status, _ := channel.historyStatus(config); status == HistoryEphemeral {
					hist = &channel.history
				}
			}
		} else {
			client := server.clients.Get(target)
			if client != nil {
				if status, _ := client.historyStatus(config); status == HistoryEphemeral {
					hist = &client.history
				}
			}
		}
	}

	if hist == nil {
		err = server.historyDB.DeleteMsgid(msgid, accountName)
	} else {
		count := hist.Delete(func(item *history.Item) bool {
			return item.Message.Msgid == msgid && (accountName == "*" || item.AccountName == accountName)
		})
		if count == 0 {
			err = errNoop
		}
	}

	return
}

// elistMatcher takes and matches ELIST conditions
type elistMatcher struct {
	MinClientsActive bool
	MinClients       int
	MaxClientsActive bool
	MaxClients       int
}

// Matches checks whether the given channel matches our matches.
func (matcher *elistMatcher) Matches(channel *Channel) bool {
	if matcher.MinClientsActive {
		if len(channel.Members()) < matcher.MinClients {
			return false
		}
	}

	if matcher.MaxClientsActive {
		if len(channel.Members()) < len(channel.members) {
			return false
		}
	}

	return true
}

var (
	infoString1 = strings.Split(`      ▄▄▄   ▄▄▄·  ▄▄ •        ▐ ▄
▪     ▀▄ █·▐█ ▀█ ▐█ ▀ ▪▪     •█▌▐█▪
 ▄█▀▄ ▐▀▀▄ ▄█▀▀█ ▄█ ▀█▄ ▄█▀▄▪▐█▐▐▌ ▄█▀▄
▐█▌.▐▌▐█•█▌▐█ ▪▐▌▐█▄▪▐█▐█▌ ▐▌██▐█▌▐█▌.▐▌
 ▀█▄▀▪.▀  ▀ ▀  ▀ ·▀▀▀▀  ▀█▄▀ ▀▀ █▪ ▀█▄▀▪

         https://oragono.io/
   https://github.com/oragono/oragono
   https://crowdin.com/project/oragono
`, "\n")
	infoString2 = strings.Split(`    Daniel Oakley,          DanielOaks,    <daniel@danieloaks.net>
    Shivaram Lingamneni,    slingamn,      <slingamn@cs.stanford.edu>
`, "\n")
	infoString3 = strings.Split(`    Jeremy Latt,            jlatt
    Edmund Huber,           edmund-huber
`, "\n")
)
