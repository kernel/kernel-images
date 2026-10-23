package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/kernel-images/server/lib/runasroot"
)

const (
	rootSupervisorConf = "/etc/supervisor/root.conf"
	rootSupervisorSock = "/run/supervisor-root.sock"
	userSupervisorDir  = "/run/kernel-supervisor"
	userServiceLogDir  = "/var/log/kernel-services"
)

// These services never execute customer commands. Their configuration and control
// socket stay root-only; the other supervisor and all its children are non-root.
func rootService(name string) bool {
	switch name {
	case "xorg", "dbus", "envoy":
		return true
	}
	return false
}

func supervisorConfig(prog string) string {
	if !runasroot.Enabled() && rootService(prog) {
		return rootSupervisorConf
	}
	return supervisorConf
}

func prepareUnprivilegedServices() error {
	u, err := user.Lookup("kernel")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	for _, dir := range []string{userSupervisorDir, userServiceLogDir, "/recordings", "/chromium", "/run/kernel", "/run/kernel-location"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
	}
	// Keep /etc root-owned. Only the indirection used for browser timezone
	// changes is writable by the browser user.
	if _, err := os.Lstat("/run/kernel-location/localtime"); os.IsNotExist(err) {
		if err := os.Symlink("/usr/share/zoneinfo/Etc/UTC", "/run/kernel-location/localtime"); err != nil {
			return err
		}
	}
	if err := os.Remove("/etc/localtime"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink("/run/kernel-location/localtime", "/etc/localtime"); err != nil {
		return err
	}

	return splitSupervisorServices("/etc/supervisor", supervisordLogD, uid, gid)
}

func splitSupervisorServices(configDir, logDir string, uid, gid int) error {
	userLogDir := filepath.Join(filepath.Dir(logDir), "kernel-services")
	if err := os.MkdirAll(userLogDir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(userLogDir, uid, gid); err != nil {
		return err
	}
	rootConfDir := filepath.Join(configDir, "root-services")
	rootSupervisorConf := filepath.Join(configDir, "root.conf")
	supervisorConfD := filepath.Join(configDir, "conf.d/services")
	supervisorConf := filepath.Join(configDir, "supervisord.conf")
	if err := os.MkdirAll(rootConfDir, 0o700); err != nil {
		return err
	}
	_, statErr := os.Stat(rootSupervisorConf)
	firstBoot := os.IsNotExist(statErr)
	if statErr != nil && !firstBoot {
		return statErr
	}
	entries, err := os.ReadDir(supervisorConfD)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// After first boot these files belong to the customer. Never read,
		// rewrite or promote them from the root startup process again.
		if !firstBoot || filepath.Ext(entry.Name()) != ".conf" {
			continue
		}
		path := filepath.Join(supervisorConfD, entry.Name())
		name := strings.TrimSuffix(entry.Name(), ".conf")
		if name == "supervisord-shim" {
			// Each supervisor publishes crash events for its own children.
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rootShim := strings.ReplaceAll(string(data), "/var/log/supervisord/supervisord-shim", "/var/log/supervisord/root-supervisord-shim")
			if err := os.WriteFile(filepath.Join(rootConfDir, entry.Name()), []byte(rootShim), 0o600); err != nil {
				return err
			}
		}
		if rootService(name) {
			if name == "xorg" {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				// The input driver's 0660 socket must be accessible to Neko,
				// including after an Xorg restart.
				conf := strings.Replace(string(data), "command=", "command=/usr/bin/setpriv --regid=kernel --clear-groups ", 1)
				if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
					return err
				}
			}
			if err := os.Rename(path, filepath.Join(rootConfDir, entry.Name())); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		conf := strings.ReplaceAll(string(data), "/var/log/supervisord/", "/var/log/kernel-services/")
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			return err
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		// Keep the existing API/log-reader paths while allowing the user
		// supervisor to rotate its logs without write access to root logs.
		logPath := filepath.Join(logDir, name)
		if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(filepath.Join(userLogDir, name), logPath); err != nil {
			return err
		}
	}
	// The user supervisor may reread its own configuration (e.g. Xvfb resize),
	// but the root supervisor never reads this directory.
	if err := os.Chown(supervisorConfD, uid, gid); err != nil {
		return err
	}
	rootConf := `[unix_http_server]
file=/run/supervisor-root.sock
chmod=0600
[supervisord]
logfile=/var/log/supervisord-root.log
pidfile=/run/supervisord-root.pid
[rpcinterface:supervisor]
supervisor.rpcinterface_factory=supervisor.rpcinterface:make_main_rpcinterface
[supervisorctl]
serverurl=unix:///run/supervisor-root.sock
[include]
files=/etc/supervisor/root-services/*.conf
`
	if err := os.WriteFile(rootSupervisorConf, []byte(rootConf), 0o600); err != nil {
		return err
	}
	userConf := `[unix_http_server]
file=/run/kernel-supervisor/supervisor.sock
chmod=0600
[supervisord]
logfile=/run/kernel-supervisor/supervisord.log
pidfile=/run/kernel-supervisor/supervisord.pid
childlogdir=/run/kernel-supervisor
[rpcinterface:supervisor]
supervisor.rpcinterface_factory=supervisor.rpcinterface:make_main_rpcinterface
[supervisorctl]
serverurl=unix:///run/kernel-supervisor/supervisor.sock
[include]
files=/etc/supervisor/conf.d/services/*.conf
`
	return os.WriteFile(supervisorConf, []byte(userConf), 0o644)
}

func userSupervisorCommand() *exec.Cmd {
	cmd := exec.Command("setpriv", "--reuid=kernel", "--regid=kernel", "--init-groups", "--no-new-privs",
		"supervisord", "-n", "-c", supervisorConf)
	cmd.Env = append(os.Environ(), "HOME=/home/kernel", "USER=kernel", "LOGNAME=kernel")
	return cmd
}
