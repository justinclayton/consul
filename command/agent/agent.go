package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/hashicorp/consul/consul"
	"github.com/hashicorp/consul/consul/structs"
	"github.com/hashicorp/serf/serf"
)

const (
	// Path to save agent service definitions
	servicesDir = "services"

	// Path to save local agent checks
	checksDir = "checks"
)

/*
 The agent is the long running process that is run on every machine.
 It exposes an RPC interface that is used by the CLI to control the
 agent. The agent runs the query interfaces like HTTP, DNS, and RPC.
 However, it can run in either a client, or server mode. In server
 mode, it runs a full Consul server. In client-only mode, it only forwards
 requests to other Consul servers.
*/
type Agent struct {
	config *Config

	// Used for writing our logs
	logger *log.Logger

	// Output sink for logs
	logOutput io.Writer

	// We have one of a client or a server, depending
	// on our configuration
	server *consul.Server
	client *consul.Client

	// state stores a local representation of the node,
	// services and checks. Used for anti-entropy.
	state localState

	// checkMonitors maps the check ID to an associated monitor
	// checkTTLs maps the check ID to an associated check TTL
	// checkLock protects updates to either
	checkMonitors map[string]*CheckMonitor
	checkTTLs     map[string]*CheckTTL
	checkLock     sync.Mutex

	// eventCh is used to receive user events
	eventCh chan serf.UserEvent

	// eventBuf stores the most recent events in a ring buffer
	// using eventIndex as the next index to insert into. This
	// is guarded by eventLock. When an insert happens, the
	// eventNotify group is notified.
	eventBuf    []*UserEvent
	eventIndex  int
	eventLock   sync.RWMutex
	eventNotify consul.NotifyGroup

	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

// Create is used to create a new Agent. Returns
// the agent or potentially an error.
func Create(config *Config, logOutput io.Writer) (*Agent, error) {
	// Ensure we have a log sink
	if logOutput == nil {
		logOutput = os.Stderr
	}

	// Validate the config
	if config.Datacenter == "" {
		return nil, fmt.Errorf("Must configure a Datacenter")
	}
	if config.DataDir == "" {
		return nil, fmt.Errorf("Must configure a DataDir")
	}

	// Try to get an advertise address
	if config.AdvertiseAddr != "" {
		if ip := net.ParseIP(config.AdvertiseAddr); ip == nil {
			return nil, fmt.Errorf("Failed to parse advertise address: %v", config.AdvertiseAddr)
		}
	} else if config.BindAddr != "0.0.0.0" && config.BindAddr != "" {
		config.AdvertiseAddr = config.BindAddr
	} else {
		ip, err := consul.GetPrivateIP()
		if err != nil {
			return nil, fmt.Errorf("Failed to get advertise address: %v", err)
		}
		config.AdvertiseAddr = ip.String()
	}

	agent := &Agent{
		config:        config,
		logger:        log.New(logOutput, "", log.LstdFlags),
		logOutput:     logOutput,
		checkMonitors: make(map[string]*CheckMonitor),
		checkTTLs:     make(map[string]*CheckTTL),
		eventCh:       make(chan serf.UserEvent, 1024),
		eventBuf:      make([]*UserEvent, 256),
		shutdownCh:    make(chan struct{}),
	}

	// Initialize the local state
	agent.state.Init(config, agent.logger)

	// Setup either the client or the server
	var err error
	if config.Server {
		err = agent.setupServer()
		agent.state.SetIface(agent.server)

		// Automatically register the "consul" service on server nodes
		consulService := structs.NodeService{
			Service: consul.ConsulServiceName,
			ID:      consul.ConsulServiceID,
			Port:    agent.config.Ports.Server,
			Tags:    []string{},
		}
		agent.state.AddService(&consulService)
	} else {
		err = agent.setupClient()
		agent.state.SetIface(agent.client)
	}
	if err != nil {
		return nil, err
	}

	// Load checks/services
	if err := agent.reloadServices(config); err != nil {
		return nil, err
	}
	if err := agent.reloadChecks(config); err != nil {
		return nil, err
	}

	// Start handling events
	go agent.handleEvents()

	// Write out the PID file if necessary
	err = agent.storePid()
	if err != nil {
		return nil, err
	}

	return agent, nil
}

// consulConfig is used to return a consul configuration
func (a *Agent) consulConfig() *consul.Config {
	// Start with the provided config or default config
	var base *consul.Config
	if a.config.ConsulConfig != nil {
		base = a.config.ConsulConfig
	} else {
		base = consul.DefaultConfig()
	}

	// Override with our config
	if a.config.Datacenter != "" {
		base.Datacenter = a.config.Datacenter
	}
	if a.config.DataDir != "" {
		base.DataDir = a.config.DataDir
	}
	if a.config.NodeName != "" {
		base.NodeName = a.config.NodeName
	}
	if a.config.BindAddr != "" {
		base.SerfLANConfig.MemberlistConfig.BindAddr = a.config.BindAddr
		base.SerfWANConfig.MemberlistConfig.BindAddr = a.config.BindAddr
	}
	if a.config.Ports.SerfLan != 0 {
		base.SerfLANConfig.MemberlistConfig.BindPort = a.config.Ports.SerfLan
		base.SerfLANConfig.MemberlistConfig.AdvertisePort = a.config.Ports.SerfLan
	}
	if a.config.Ports.SerfWan != 0 {
		base.SerfWANConfig.MemberlistConfig.BindPort = a.config.Ports.SerfWan
		base.SerfWANConfig.MemberlistConfig.AdvertisePort = a.config.Ports.SerfWan
	}
	if a.config.BindAddr != "" {
		bindAddr := &net.TCPAddr{
			IP:   net.ParseIP(a.config.BindAddr),
			Port: a.config.Ports.Server,
		}
		base.RPCAddr = bindAddr
	}
	if a.config.AdvertiseAddr != "" {
		base.SerfLANConfig.MemberlistConfig.AdvertiseAddr = a.config.AdvertiseAddr
		base.SerfWANConfig.MemberlistConfig.AdvertiseAddr = a.config.AdvertiseAddr
		base.RPCAdvertise = &net.TCPAddr{
			IP:   net.ParseIP(a.config.AdvertiseAddr),
			Port: a.config.Ports.Server,
		}
	}
	if a.config.Bootstrap {
		base.Bootstrap = true
	}
	if a.config.RejoinAfterLeave {
		base.RejoinAfterLeave = true
	}
	if a.config.BootstrapExpect != 0 {
		base.BootstrapExpect = a.config.BootstrapExpect
	}
	if a.config.Protocol > 0 {
		base.ProtocolVersion = uint8(a.config.Protocol)
	}
	if a.config.ACLToken != "" {
		base.ACLToken = a.config.ACLToken
	}
	if a.config.ACLMasterToken != "" {
		base.ACLMasterToken = a.config.ACLMasterToken
	}
	if a.config.ACLDatacenter != "" {
		base.ACLDatacenter = a.config.ACLDatacenter
	}
	if a.config.ACLTTLRaw != "" {
		base.ACLTTL = a.config.ACLTTL
	}
	if a.config.ACLDefaultPolicy != "" {
		base.ACLDefaultPolicy = a.config.ACLDefaultPolicy
	}
	if a.config.ACLDownPolicy != "" {
		base.ACLDownPolicy = a.config.ACLDownPolicy
	}

	// Format the build string
	revision := a.config.Revision
	if len(revision) > 8 {
		revision = revision[:8]
	}
	base.Build = fmt.Sprintf("%s%s:%s",
		a.config.Version, a.config.VersionPrerelease, revision)

	// Copy the TLS configuration
	base.VerifyIncoming = a.config.VerifyIncoming
	base.VerifyOutgoing = a.config.VerifyOutgoing
	base.CAFile = a.config.CAFile
	base.CertFile = a.config.CertFile
	base.KeyFile = a.config.KeyFile
	base.ServerName = a.config.ServerName

	// Setup the ServerUp callback
	base.ServerUp = a.state.ConsulServerUp

	// Setup the user event callback
	base.UserEventHandler = func(e serf.UserEvent) {
		select {
		case a.eventCh <- e:
		case <-a.shutdownCh:
		}
	}

	// Setup the loggers
	base.LogOutput = a.logOutput
	return base
}

// setupServer is used to initialize the Consul server
func (a *Agent) setupServer() error {
	config := a.consulConfig()

	if err := a.setupKeyrings(config); err != nil {
		return fmt.Errorf("Failed to configure keyring: %v", err)
	}

	server, err := consul.NewServer(config)
	if err != nil {
		return fmt.Errorf("Failed to start Consul server: %v", err)
	}
	a.server = server
	return nil
}

// setupClient is used to initialize the Consul client
func (a *Agent) setupClient() error {
	config := a.consulConfig()

	if err := a.setupKeyrings(config); err != nil {
		return fmt.Errorf("Failed to configure keyring: %v", err)
	}

	client, err := consul.NewClient(config)
	if err != nil {
		return fmt.Errorf("Failed to start Consul client: %v", err)
	}
	a.client = client
	return nil
}

// setupKeyrings is used to initialize and load keyrings during agent startup
func (a *Agent) setupKeyrings(config *consul.Config) error {
	fileLAN := filepath.Join(a.config.DataDir, serfLANKeyring)
	fileWAN := filepath.Join(a.config.DataDir, serfWANKeyring)

	if a.config.EncryptKey == "" {
		goto LOAD
	}
	if _, err := os.Stat(fileLAN); err != nil {
		if err := initKeyring(fileLAN, a.config.EncryptKey); err != nil {
			return err
		}
	}
	if a.config.Server {
		if _, err := os.Stat(fileWAN); err != nil {
			if err := initKeyring(fileWAN, a.config.EncryptKey); err != nil {
				return err
			}
		}
	}

LOAD:
	if _, err := os.Stat(fileLAN); err == nil {
		config.SerfLANConfig.KeyringFile = fileLAN
	}
	if err := loadKeyringFile(config.SerfLANConfig); err != nil {
		return err
	}
	if a.config.Server {
		if _, err := os.Stat(fileWAN); err == nil {
			config.SerfWANConfig.KeyringFile = fileWAN
		}
		if err := loadKeyringFile(config.SerfWANConfig); err != nil {
			return err
		}
	}

	// Success!
	return nil
}

// RPC is used to make an RPC call to the Consul servers
// This allows the agent to implement the Consul.Interface
func (a *Agent) RPC(method string, args interface{}, reply interface{}) error {
	if a.server != nil {
		return a.server.RPC(method, args, reply)
	}
	return a.client.RPC(method, args, reply)
}

// Leave is used to prepare the agent for a graceful shutdown
func (a *Agent) Leave() error {
	if a.server != nil {
		return a.server.Leave()
	} else {
		return a.client.Leave()
	}
}

// Shutdown is used to hard stop the agent. Should be
// preceded by a call to Leave to do it gracefully.
func (a *Agent) Shutdown() error {
	a.shutdownLock.Lock()
	defer a.shutdownLock.Unlock()

	if a.shutdown {
		return nil
	}

	// Stop all the checks
	a.checkLock.Lock()
	defer a.checkLock.Unlock()
	for _, chk := range a.checkMonitors {
		chk.Stop()
	}
	for _, chk := range a.checkTTLs {
		chk.Stop()
	}

	a.logger.Println("[INFO] agent: requesting shutdown")
	var err error
	if a.server != nil {
		err = a.server.Shutdown()
	} else {
		err = a.client.Shutdown()
	}

	pidErr := a.deletePid()
	if pidErr != nil {
		a.logger.Println("[WARN] agent: could not delete pid file ", pidErr)
	}

	a.logger.Println("[INFO] agent: shutdown complete")
	a.shutdown = true
	close(a.shutdownCh)
	return err
}

// ShutdownCh is used to return a channel that can be
// selected to wait for the agent to perform a shutdown.
func (a *Agent) ShutdownCh() <-chan struct{} {
	return a.shutdownCh
}

// JoinLAN is used to have the agent join a LAN cluster
func (a *Agent) JoinLAN(addrs []string) (n int, err error) {
	a.logger.Printf("[INFO] agent: (LAN) joining: %v", addrs)
	if a.server != nil {
		n, err = a.server.JoinLAN(addrs)
	} else {
		n, err = a.client.JoinLAN(addrs)
	}
	a.logger.Printf("[INFO] agent: (LAN) joined: %d Err: %v", n, err)
	return
}

// JoinWAN is used to have the agent join a WAN cluster
func (a *Agent) JoinWAN(addrs []string) (n int, err error) {
	a.logger.Printf("[INFO] agent: (WAN) joining: %v", addrs)
	if a.server != nil {
		n, err = a.server.JoinWAN(addrs)
	} else {
		err = fmt.Errorf("Must be a server to join WAN cluster")
	}
	a.logger.Printf("[INFO] agent: (WAN) joined: %d Err: %v", n, err)
	return
}

// ForceLeave is used to remove a failed node from the cluster
func (a *Agent) ForceLeave(node string) (err error) {
	a.logger.Printf("[INFO] Force leaving node: %v", node)
	if a.server != nil {
		err = a.server.RemoveFailedNode(node)
	} else {
		err = a.client.RemoveFailedNode(node)
	}
	if err != nil {
		a.logger.Printf("[WARN] Failed to remove node: %v", err)
	}
	return err
}

// LocalMember is used to return the local node
func (a *Agent) LocalMember() serf.Member {
	if a.server != nil {
		return a.server.LocalMember()
	} else {
		return a.client.LocalMember()
	}
}

// LANMembers is used to retrieve the LAN members
func (a *Agent) LANMembers() []serf.Member {
	if a.server != nil {
		return a.server.LANMembers()
	} else {
		return a.client.LANMembers()
	}
}

// WANMembers is used to retrieve the WAN members
func (a *Agent) WANMembers() []serf.Member {
	if a.server != nil {
		return a.server.WANMembers()
	} else {
		return nil
	}
}

// StartSync is called once Services and Checks are registered.
// This is called to prevent a race between clients and the anti-entropy routines
func (a *Agent) StartSync() {
	// Start the anti entropy routine
	go a.state.antiEntropy(a.shutdownCh)
}

// PauseSync is used to pause anti-entropy while bulk changes are make
func (a *Agent) PauseSync() {
	a.state.Pause()
}

// ResumeSync is used to unpause anti-entropy after bulk changes are make
func (a *Agent) ResumeSync() {
	a.state.Resume()
}

// persistService saves a service definition to a JSON file in the data dir
func (a *Agent) persistService(service *structs.NodeService) error {
	svcPath := filepath.Join(a.config.DataDir, servicesDir, service.ID)
	if _, err := os.Stat(svcPath); os.IsNotExist(err) {
		encoded, err := json.Marshal(service)
		if err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(svcPath), 0700); err != nil {
			return err
		}
		fh, err := os.OpenFile(svcPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer fh.Close()
		if _, err := fh.Write(encoded); err != nil {
			return err
		}
	}
	return nil
}

// purgeService removes a persisted service definition file from the data dir
func (a *Agent) purgeService(serviceID string) error {
	svcPath := filepath.Join(a.config.DataDir, servicesDir, serviceID)
	if _, err := os.Stat(svcPath); err == nil {
		return os.Remove(svcPath)
	}
	return nil
}

// restoreServices is used to load previously persisted service definitions
// into the agent during startup.
func (a *Agent) restoreServices() error {
	svcDir := filepath.Join(a.config.DataDir, servicesDir)
	if _, err := os.Stat(svcDir); os.IsNotExist(err) {
		return nil
	}

	err := filepath.Walk(svcDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Name() == servicesDir {
			return nil
		}
		fh, err := os.Open(filepath.Join(svcDir, fi.Name()))
		if err != nil {
			return err
		}
		content := make([]byte, fi.Size())
		if _, err := fh.Read(content); err != nil {
			return err
		}

		var svc *structs.NodeService
		if err := json.Unmarshal(content, &svc); err != nil {
			return err
		}

		if _, ok := a.state.services[svc.ID]; ok {
			// Purge previously persisted service. This allows config to be
			// preferred over services persisted from the API.
			a.logger.Printf("[DEBUG] Service %s exists, not restoring", svc.ID)
			return a.purgeService(svc.ID)
		} else {
			a.logger.Printf("[DEBUG] Restored service definition: %s", svc.ID)
			return a.AddService(svc, nil, false)
		}
	})
	return err
}

// persistCheck saves a check definition to the local agent's state directory
func (a *Agent) persistCheck(check *structs.HealthCheck, chkType *CheckType) error {
	checkPath := filepath.Join(a.config.DataDir, checksDir, check.CheckID)
	if _, err := os.Stat(checkPath); !os.IsNotExist(err) {
		return err
	}

	// Create the persisted check
	p := persistedCheck{check, chkType}

	encoded, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(checkPath), 0700); err != nil {
		return err
	}
	fh, err := os.OpenFile(checkPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Write(encoded); err != nil {
		return err
	}
	return nil
}

// purgeCheck removes a persisted check definition file from the data dir
func (a *Agent) purgeCheck(checkID string) error {
	checkPath := filepath.Join(a.config.DataDir, checksDir, checkID)
	if _, err := os.Stat(checkPath); err == nil {
		return os.Remove(checkPath)
	}
	return nil
}

// restoreChecks is used to load previously persisted health check definitions
// into the agent during startup.
func (a *Agent) restoreChecks() error {
	checkDir := filepath.Join(a.config.DataDir, checksDir)
	if _, err := os.Stat(checkDir); os.IsNotExist(err) {
		return nil
	}

	err := filepath.Walk(checkDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Name() == checksDir {
			return nil
		}
		fh, err := os.Open(filepath.Join(checkDir, fi.Name()))
		if err != nil {
			return err
		}
		content := make([]byte, fi.Size())
		if _, err := fh.Read(content); err != nil {
			return err
		}

		var p persistedCheck
		if err := json.Unmarshal(content, &p); err != nil {
			return err
		}

		if _, ok := a.state.checks[p.Check.CheckID]; ok {
			// Purge previously persisted check. This allows config to be
			// preferred over persisted checks from the API.
			a.logger.Printf("[DEBUG] Check %s exists, not restoring", p.Check.CheckID)
			return a.purgeCheck(p.Check.CheckID)
		} else {
			// Default check to critical to avoid placing potentially unhealthy
			// services into the active pool
			p.Check.Status = structs.HealthCritical

			a.logger.Printf("[DEBUG] Restored health check: %s", p.Check.CheckID)
			return a.AddCheck(p.Check, p.ChkType, false)
		}
	})
	return err
}

// AddService is used to add a service entry.
// This entry is persistent and the agent will make a best effort to
// ensure it is registered
func (a *Agent) AddService(service *structs.NodeService, chkType *CheckType, persist bool) error {
	if service.Service == "" {
		return fmt.Errorf("Service name missing")
	}
	if service.ID == "" && service.Service != "" {
		service.ID = service.Service
	}
	if chkType != nil && !chkType.Valid() {
		return fmt.Errorf("Check type is not valid")
	}

	// Add the service
	a.state.AddService(service)

	// Persist the service to a file
	if persist {
		if err := a.persistService(service); err != nil {
			return err
		}
	}

	// Create an associated health check
	if chkType != nil {
		check := &structs.HealthCheck{
			Node:        a.config.NodeName,
			CheckID:     fmt.Sprintf("service:%s", service.ID),
			Name:        fmt.Sprintf("Service '%s' check", service.Service),
			Status:      structs.HealthCritical,
			Notes:       chkType.Notes,
			ServiceID:   service.ID,
			ServiceName: service.Service,
		}
		if err := a.AddCheck(check, chkType, persist); err != nil {
			return err
		}
	}
	return nil
}

// RemoveService is used to remove a service entry.
// The agent will make a best effort to ensure it is deregistered
func (a *Agent) RemoveService(serviceID string, persist bool) error {
	// Protect "consul" service from deletion by a user
	if a.server != nil && serviceID == consul.ConsulServiceID {
		return fmt.Errorf(
			"Deregistering the %s service is not allowed",
			consul.ConsulServiceID)
	}

	// Remove service immeidately
	a.state.RemoveService(serviceID)

	// Remove the service from the data dir
	if persist {
		if err := a.purgeService(serviceID); err != nil {
			return err
		}
	}

	// Deregister any associated health checks
	checkID := fmt.Sprintf("service:%s", serviceID)
	return a.RemoveCheck(checkID, persist)
}

// AddCheck is used to add a health check to the agent.
// This entry is persistent and the agent will make a best effort to
// ensure it is registered. The Check may include a CheckType which
// is used to automatically update the check status
func (a *Agent) AddCheck(check *structs.HealthCheck, chkType *CheckType, persist bool) error {
	if check.CheckID == "" {
		return fmt.Errorf("CheckID missing")
	}
	if chkType != nil && !chkType.Valid() {
		return fmt.Errorf("Check type is not valid")
	}

	a.checkLock.Lock()
	defer a.checkLock.Unlock()

	// Check if already registered
	if chkType != nil {
		if chkType.IsTTL() {
			if existing, ok := a.checkTTLs[check.CheckID]; ok {
				existing.Stop()
			}

			ttl := &CheckTTL{
				Notify:  &a.state,
				CheckID: check.CheckID,
				TTL:     chkType.TTL,
				Logger:  a.logger,
			}
			ttl.Start()
			a.checkTTLs[check.CheckID] = ttl

		} else {
			if existing, ok := a.checkMonitors[check.CheckID]; ok {
				existing.Stop()
			}
			if chkType.Interval < MinInterval {
				a.logger.Println(fmt.Sprintf("[WARN] agent: check '%s' has interval below minimum of %v",
					check.CheckID, MinInterval))
				chkType.Interval = MinInterval
			}

			monitor := &CheckMonitor{
				Notify:   &a.state,
				CheckID:  check.CheckID,
				Script:   chkType.Script,
				Interval: chkType.Interval,
				Logger:   a.logger,
			}
			monitor.Start()
			a.checkMonitors[check.CheckID] = monitor
		}
	}

	// Add to the local state for anti-entropy
	a.state.AddCheck(check)

	// Persist the check
	if persist {
		return a.persistCheck(check, chkType)
	}

	return nil
}

// RemoveCheck is used to remove a health check.
// The agent will make a best effort to ensure it is deregistered
func (a *Agent) RemoveCheck(checkID string, persist bool) error {
	// Add to the local state for anti-entropy
	a.state.RemoveCheck(checkID)

	a.checkLock.Lock()
	defer a.checkLock.Unlock()

	// Stop any monitors
	if check, ok := a.checkMonitors[checkID]; ok {
		check.Stop()
		delete(a.checkMonitors, checkID)
	}
	if check, ok := a.checkTTLs[checkID]; ok {
		check.Stop()
		delete(a.checkTTLs, checkID)
	}
	if persist {
		return a.purgeCheck(checkID)
	}
	return nil
}

// UpdateCheck is used to update the status of a check.
// This can only be used with checks of the TTL type.
func (a *Agent) UpdateCheck(checkID, status, output string) error {
	a.checkLock.Lock()
	defer a.checkLock.Unlock()

	check, ok := a.checkTTLs[checkID]
	if !ok {
		return fmt.Errorf("CheckID does not have associated TTL")
	}

	// Set the status through CheckTTL to reset the TTL
	check.SetStatus(status, output)
	return nil
}

// Stats is used to get various debugging state from the sub-systems
func (a *Agent) Stats() map[string]map[string]string {
	toString := func(v uint64) string {
		return strconv.FormatUint(v, 10)
	}
	var stats map[string]map[string]string
	if a.server != nil {
		stats = a.server.Stats()
	} else {
		stats = a.client.Stats()
	}
	stats["agent"] = map[string]string{
		"check_monitors": toString(uint64(len(a.checkMonitors))),
		"check_ttls":     toString(uint64(len(a.checkTTLs))),
		"checks":         toString(uint64(len(a.state.checks))),
		"services":       toString(uint64(len(a.state.services))),
	}

	revision := a.config.Revision
	if len(revision) > 8 {
		revision = revision[:8]
	}
	stats["build"] = map[string]string{
		"revision":   revision,
		"version":    a.config.Version,
		"prerelease": a.config.VersionPrerelease,
	}
	return stats
}

// storePid is used to write out our PID to a file if necessary
func (a *Agent) storePid() error {
	// Quit fast if no pidfile
	pidPath := a.config.PidFile
	if pidPath == "" {
		return nil
	}

	// Open the PID file
	pidFile, err := os.OpenFile(pidPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("Could not open pid file: %v", err)
	}
	defer pidFile.Close()

	// Write out the PID
	pid := os.Getpid()
	_, err = pidFile.WriteString(fmt.Sprintf("%d", pid))
	if err != nil {
		return fmt.Errorf("Could not write to pid file: %s", err)
	}
	return nil
}

// deletePid is used to delete our PID on exit
func (a *Agent) deletePid() error {
	// Quit fast if no pidfile
	pidPath := a.config.PidFile
	if pidPath == "" {
		return nil
	}

	stat, err := os.Stat(pidPath)
	if err != nil {
		return fmt.Errorf("Could not remove pid file: %s", err)
	}

	if stat.IsDir() {
		return fmt.Errorf("Specified pid file path is directory")
	}

	err = os.Remove(pidPath)
	if err != nil {
		return fmt.Errorf("Could not remove pid file: %s", err)
	}
	return nil
}

// reloadServices reloads all known services from config and state. It is used
// at initial agent startup as well as during config reloads.
func (a *Agent) reloadServices(conf *Config) error {
	for _, service := range a.state.Services() {
		if service.ID == consul.ConsulServiceID {
			continue
		}
		if err := a.RemoveService(service.ID, false); err != nil {
			return fmt.Errorf("Failed deregistering service '%s': %v", service.ID, err)
		}
	}

	// Register the services from config
	for _, service := range conf.Services {
		ns := service.NodeService()
		chkType := service.CheckType()
		if err := a.AddService(ns, chkType, false); err != nil {
			return fmt.Errorf("Failed to register service '%s': %v", service.ID, err)
		}
	}

	// Load any persisted services
	if err := a.restoreServices(); err != nil {
		return fmt.Errorf("Failed restoring services: %s", err)
	}

	return nil
}

// reloadChecks reloads all known checks from config and state. It can be used
// during initial agent start or for config reloads.
func (a *Agent) reloadChecks(conf *Config) error {
	for _, check := range a.state.Checks() {
		if err := a.RemoveCheck(check.CheckID, false); err != nil {
			return fmt.Errorf("Failed deregistering check '%s': %s", check.CheckID, err)
		}
	}

	// Register the checks from config
	for _, check := range conf.Checks {
		health := check.HealthCheck(conf.NodeName)
		chkType := &check.CheckType
		if err := a.AddCheck(health, chkType, false); err != nil {
			return fmt.Errorf("Failed to register check '%s': %v %v", check.Name, err, check)
		}
	}

	// Load any persisted checks
	if err := a.restoreChecks(); err != nil {
		return fmt.Errorf("Failed restoring checks: %s", err)
	}

	return nil
}
