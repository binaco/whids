package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/0xrawsec/gene/v2/engine"
	"github.com/0xrawsec/whids/api"
	"github.com/0xrawsec/whids/hids"
	"github.com/0xrawsec/whids/utils"
	"github.com/pelletier/go-toml"
	"golang.org/x/sys/windows/svc"

	"github.com/0xrawsec/golang-utils/crypto/data"
	"github.com/0xrawsec/golang-utils/fsutil"

	"github.com/0xrawsec/golang-utils/log"
)

const (
	exitFail    = 1
	exitSuccess = 0
	banner      = `
	██╗    ██╗██╗  ██╗██╗██████╗ ███████╗
	██║    ██║██║  ██║██║██╔══██╗██╔════╝
	██║ █╗ ██║███████║██║██║  ██║███████╗
	██║███╗██║██╔══██║██║██║  ██║╚════██║
	╚███╔███╔╝██║  ██║██║██████╔╝███████║
	 ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝╚═════╝ ╚══════╝
	           Windows Host IDS
	`
	copyright = "WHIDS Copyright (C) 2017 RawSec SARL (@0xrawsec)"
	license   = `AGPLv3: This program comes with ABSOLUTELY NO WARRANTY.`

	svcName = "WHIDS"
)

var (
	abs, _ = filepath.Abs(filepath.Dir(os.Args[0]))

	logDir = filepath.Join(abs, "Logs")

	// DefaultHIDSConfig is the default HIDS configuration
	DefaultHIDSConfig = hids.Config{
		RulesConfig: &hids.RulesConfig{
			RulesDB:        filepath.Join(abs, "Database", "Rules"),
			ContainersDB:   filepath.Join(abs, "Database", "Containers"),
			UpdateInterval: 60 * time.Second,
		},

		FwdConfig: &api.ForwarderConfig{
			Local: true,
			Client: api.ClientConfig{
				MaxUploadSize: api.DefaultMaxUploadSize,
			},
			Logging: api.LoggingConfig{
				Dir:              filepath.Join(logDir, "Alerts"),
				RotationInterval: time.Hour * 5,
			},
		},
		EtwConfig: &hids.EtwConfig{
			Providers: []string{
				"Microsoft-Windows-Sysmon",
				"Microsoft-Windows-Windows Defender",
				"Microsoft-Windows-PowerShell",
				"Microsoft-Antimalware-Scan-Interface",
			},
			Traces: []string{"Eventlog-Security"},
		},
		Sysmon: &hids.SysmonConfig{
			Bin:              "C:\\Windows\\Sysmon64.exe",
			ArchiveDirectory: "C:\\Sysmon\\",
			CleanArchived:    true,
		},
		Actions: &hids.ActionsConfig{
			AvailableActions: hids.AvailableActions,
			Low:              []string{},
			Medium:           []string{"brief", "filedump", "regdump"},
			High:             []string{"report", "filedump", "regdump"},
			Critical:         []string{"report", "filedump", "regdump", "memdump"},
		},
		Dump: &hids.DumpConfig{
			Dir:           filepath.Join(abs, "Dumps"),
			Compression:   true,
			MaxDumps:      4,
			DumpUntracked: false,
		},
		Report: &hids.ReportConfig{
			EnableReporting: false,
			OSQuery: hids.OSQueryConfig{
				Bin:    "C:\\Program Files\\osquery\\osqueryi.exe",
				Tables: []string{"processes", "services", "scheduled_tasks", "drivers", "startup_items", "process_open_sockets"}},
			Commands: []hids.ReportCommand{{
				Description: "Example command",
				Name:        "osqueryi.exe",
				Args:        []string{"--json", "-A", "processes"},
				ExpectJSON:  true,
			}},
			CommandTimeout: 60 * time.Second,
		},
		AuditConfig: &hids.AuditConfig{
			AuditPolicies: []string{"File System"},
		},
		CanariesConfig: &hids.CanariesConfig{
			Enable: false,
			Canaries: []*hids.Canary{
				{
					Directories: []string{"$SYSTEMDRIVE", "$SYSTEMROOT"},
					Files:       []string{"readme.pdf", "readme.docx", "readme.txt"},
					Delete:      true,
				},
			},
			Actions: []string{"kill", "memdump", "filedump", "blacklist", "report"},
			Whitelist: []string{
				"System",
				"C:\\Windows\\explorer.exe",
			},
		},
		CritTresh:       5,
		Logfile:         filepath.Join(logDir, "whids.log"),
		EnableHooks:     true,
		EnableFiltering: true,
		Endpoint:        true,
		LogAll:          false}
)

var (
	flagDumpConfig bool
	flagConfigure  bool
	flagInstall    bool
	flagUninstall  bool
	flagDryRun     bool
	flagPrintAll   bool
	flagDebug      bool
	flagVersion    bool
	flagProfile    bool
	flagRestore    bool
	flagAutologger bool

	hostIDS *hids.HIDS

	importRules string

	config = filepath.Join(abs, "config.toml")

	osSignals = make(chan os.Signal)
)

func printInfo(writer io.Writer) {
	fmt.Fprintf(writer, "%s\nVersion: %s (commit: %s)\nCopyright: %s\nLicense: %s\n\n", banner, version, commitID, copyright, license)
}

func configure() error {
	var writer *os.File
	var err error

	if writer, err = utils.HidsCreateFile(config); err != nil {
		return err
	}
	defer writer.Close()

	enc := toml.NewEncoder(writer)
	enc.Order(toml.OrderPreserve)
	if err := enc.Encode(DefaultHIDSConfig); err != nil {
		return err
	}
	return nil
}

func updateAutologger(c *hids.Config) error {
	if err := c.EtwConfig.ConfigureAutologger(); err != nil {
		return err
	}
	return nil
}

func restoreCanaries(c *hids.Config) {
	// Removing ACLs found in config
	log.Infof("Restoring global File System Audit ACLs")
	c.AuditConfig.Restore()

	log.Infof("Restoring canary File System Audit ACLs")
	c.CanariesConfig.RestoreACLs()
}

func cleanCanaries(c *hids.Config) {
	restoreCanaries(c)

	log.Infof("Deleting canary files")
	c.CanariesConfig.Clean()
}

func deleteAutologger() error {
	return hids.Autologger.Delete()
}

func runHids(service bool) {
	var err error
	var hidsConf hids.Config

	log.Infof("Running HIDS as Windows service: %t", service)

	hidsConf, err = hids.LoadsHIDSConfig(config)
	if err != nil {
		log.Abort(exitFail, fmt.Errorf("failed to load configuration: %s", err))
	}

	hostIDS, err = hids.NewHIDS(&hidsConf)
	if err != nil {
		log.Abort(exitFail, fmt.Errorf("failed to create HIDS: %s", err))
	}

	hostIDS.DryRun = flagDryRun
	hostIDS.PrintAll = flagPrintAll

	// If not a service we need to be able to stop the HIDS
	if !service {
		// Register SIGINT handler to stop listening on channels
		signal.Notify(osSignals, os.Interrupt)
		go func() {
			<-osSignals
			log.Infof("Received SIGINT")
			// runs stop on sigint
			hostIDS.Stop()
		}()
	}

	// Runs HIDS and wait for the output
	hostIDS.Run()
	if !service {
		hostIDS.Wait()
	}
}

func proctectDir(dir string) {
	var out []byte
	var err error

	// we first need to reset the ACLs otherwise next command does not work
	cmd := []string{"icacls", dir, "/reset"}
	if out, err = exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		log.Errorf("Failed to reset installation directory ACLs: %s", err)
		log.Errorf("icacls output: %s", string(out))
		return
	}

	// we grant Administrators and SYSTEM full access rights
	cmd = []string{"icacls", dir, "/inheritance:r", "/grant:r", "Administrators:(OI)(CI)F", "/grant:r", "SYSTEM:(OI)(CI)F"}
	if out, err = exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
		log.Errorf("Failed to protect installation directory with ACLs: %s", err)
		log.Errorf("icacls output: %s", string(out))
		return
	}

	log.Infof("Successfully protected installation directory with ACLs")
}

func main() {

	flag.BoolVar(&flagDumpConfig, "dump-conf", flagDumpConfig, "Dumps default configuration to stdout")
	flag.BoolVar(&flagInstall, "install", flagInstall, "Install EDR")
	flag.BoolVar(&flagAutologger, "autologger", flagAutologger, "Update EDR's ETW autologger configuration")
	flag.BoolVar(&flagUninstall, "uninstall", flagUninstall, "Uninstall EDR")
	flag.BoolVar(&flagDryRun, "dry", flagDryRun, "Dry run (do everything except listening on channels)")
	flag.BoolVar(&flagPrintAll, "all", flagPrintAll, "Print all events passing through HIDS")
	flag.BoolVar(&flagVersion, "v", flagVersion, "Print version information and exit")
	flag.BoolVar(&flagProfile, "prof", flagProfile, "Profile program")
	flag.BoolVar(&flagDebug, "d", flagDebug, "Enable debugging messages")
	flag.BoolVar(&flagRestore, "restore", flagRestore, "Restore Audit Policies and File System Audit ACLs according to configuration file")
	flag.StringVar(&config, "c", config, "Configuration file")
	flag.StringVar(&importRules, "import", importRules, "Import rules")

	flag.Usage = func() {
		printInfo(os.Stderr)
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
		os.Exit(exitSuccess)
	}

	flag.Parse()

	isIntSess, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Abort(exitFail, fmt.Errorf("failed to determine if we are running in an interactive session: %v", err))
	}

	if flagInstall || flagAutologger {

		// Only when installing
		if flagInstall {
			// dump configuration first as config is needed
			// by subsequent functions
			if err := configure(); err != nil {
				log.Errorf("Failed to build configuration: %s", err)
				os.Exit(exitFail)
			}
		}

		conf, err := hids.LoadsHIDSConfig(config)
		if err != nil {
			log.Errorf("Failed to load configuration: %s", err)
			os.Exit(exitFail)
		}

		if err := deleteAutologger(); err != nil {
			log.Errorf("Failed to delete autologger: %s", err)
			os.Exit(exitFail)
		}

		if err := updateAutologger(&conf); err != nil {
			log.Errorf("Failed to update autologger: %s", err)
			os.Exit(exitFail)
		}

		os.Exit(exitSuccess)
	}

	if flagUninstall {
		// we should not abort uninstallation if error
		var conf hids.Config

		rc := exitSuccess

		if conf, err = hids.LoadsHIDSConfig(config); err == nil {
			cleanCanaries(&conf)
		} else {
			log.Errorf("Failed to load configuration: %s", err)
			rc = exitFail
		}

		if err := deleteAutologger(); err != nil {
			log.Errorf("Failed to delete autologger: %s", err)
			rc = exitFail
		}

		os.Exit(rc)
	}

	// profile the program
	if flagProfile {
		go func() {
			log.Info("Running profiling server", http.ListenAndServe("0.0.0.0:4242", nil))
		}()
	}

	// Print version information and exit
	if flagVersion {
		printInfo(os.Stderr)
		os.Exit(exitSuccess)
	}

	if flagDumpConfig || flagConfigure {
		writer := os.Stdout
		enc := toml.NewEncoder(writer)
		enc.Order(toml.OrderPreserve)
		if err := enc.Encode(DefaultHIDSConfig); err != nil {
			log.Abort(exitFail, err)
		}
		os.Exit(exitSuccess)
	}

	// Enabling debug if needed
	if flagDebug {
		log.InitLogger(log.LDebug)
	}

	hidsConf, err := hids.LoadsHIDSConfig(config)
	if err != nil {
		log.Abort(exitFail, fmt.Sprintf("Failed to load configuration: %s", err))
	}

	if flagRestore {
		restoreCanaries(&hidsConf)
		os.Exit(exitSuccess)
	}

	// has to be there so that we print logs to stdout
	if importRules != "" {
		// in order not to write logs into file
		// TODO: add a stream handler to log facility
		hidsConf.Logfile = ""
		log.Infof("Importing rules from %s", importRules)
		eng := engine.NewEngine()
		eng.SetDumpRaw(true)

		if err := eng.LoadDirectory(importRules); err != nil {
			log.Abort(exitFail, fmt.Sprintf("Failed to import rules: %s", err))
		}

		prules, psha256 := hidsConf.RulesConfig.RulesPaths()
		rules := new(bytes.Buffer)
		for rule := range eng.GetRawRule(".*") {
			if _, err := rules.Write([]byte(rule + "\n")); err != nil {
				log.Abort(exitFail, fmt.Sprintf("Failed to import rules: %s", err))
			}
		}

		if err := ioutil.WriteFile(prules, rules.Bytes(), utils.DefaultPerms); err != nil {
			log.Abort(exitFail, fmt.Sprintf("Failed to import rules: %s", err))
		}

		if err := ioutil.WriteFile(psha256, []byte(data.Sha256(rules.Bytes())), utils.DefaultPerms); err != nil {
			log.Abort(exitFail, fmt.Sprintf("Failed to import rules: %s", err))
		}

		log.Infof("IMPORT SUCCESSFUL: %s", prules)
		os.Exit(0)
	}

	// If it is called by the Windows Service Manager (not interactive)
	if !isIntSess {
		// set logfile the time the service starts
		log.SetLogfile(filepath.Join(abs, "bootstrap.log"))

		// if running as service we protect installation directory with appropriate ACLs
		if fsutil.IsDir(abs) {
			proctectDir(abs)
		}
		runService(svcName, false)
		return
	} else {
		runHids(false)
		hostIDS.LogStats()
	}
}
