package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupervisorRouting(t *testing.T) {
	for _, mode := range []string{"", "false", "true"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RUN_AS_ROOT", mode)
			for _, service := range []string{"chromium", "chromedriver", "kernel-images-api", "neko", "openbox", "xvfb", "pulseaudio", "supervisord-shim"} {
				require.Equal(t, supervisorConf, supervisorConfig(service))
			}
			for _, service := range []string{"xorg", "dbus", "envoy"} {
				want := rootSupervisorConf
				if mode == "true" {
					want = supervisorConf
				}
				require.Equal(t, want, supervisorConfig(service))
			}
		})
	}
}

func TestSplitSupervisorServices(t *testing.T) {
	for _, profile := range []string{"chromium-headful", "chromium-headless/image"} {
		t.Run(profile, func(t *testing.T) {
			dir := t.TempDir()
			services := filepath.Join(dir, "conf.d/services")
			logs := filepath.Join(dir, "logs")
			require.NoError(t, os.MkdirAll(services, 0o755))
			require.NoError(t, os.Mkdir(logs, 0o755))
			repo := filepath.Join("..", "..", "..")
			configs, err := filepath.Glob(filepath.Join(repo, "images", profile, "supervisor/services/*.conf"))
			require.NoError(t, err)
			require.NotEmpty(t, configs)
			for _, config := range configs {
				data, err := os.ReadFile(config)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(services, filepath.Base(config)), data, 0o644))
			}
			envoy, err := os.ReadFile(filepath.Join(repo, "shared/envoy/supervisor-envoy.conf"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(services, "envoy.conf"), envoy, 0o644))

			require.NoError(t, splitSupervisorServices(dir, logs, os.Getuid(), os.Getgid()))
			for _, service := range []string{"dbus", "envoy"} {
				require.FileExists(t, filepath.Join(dir, "root-services", service+".conf"))
				require.NoFileExists(t, filepath.Join(services, service+".conf"))
			}
			if profile == "chromium-headful" {
				data, err := os.ReadFile(filepath.Join(dir, "root-services/xorg.conf"))
				require.NoError(t, err)
				require.Contains(t, string(data), "command=/usr/bin/setpriv --regid=kernel --clear-groups /usr/bin/Xorg")
				require.NoFileExists(t, filepath.Join(services, "xorg.conf"))
				require.FileExists(t, filepath.Join(services, "openbox.conf"))
				require.FileExists(t, filepath.Join(services, "neko.conf"))
			} else {
				require.FileExists(t, filepath.Join(services, "xvfb.conf"))
				require.NoFileExists(t, filepath.Join(dir, "root-services/xvfb.conf"))
			}
			data, err := os.ReadFile(filepath.Join(services, "chromium.conf"))
			require.NoError(t, err)
			require.Contains(t, string(data), "stdout_logfile=/var/log/kernel-services/chromium")
			require.NotContains(t, string(data), "stdout_logfile_maxbytes=0")
			logTarget, err := os.Readlink(filepath.Join(logs, "chromium"))
			require.NoError(t, err)
			require.Equal(t, filepath.Join(dir, "kernel-services/chromium"), logTarget)
			data, err = os.ReadFile(filepath.Join(dir, "root-services/supervisord-shim.conf"))
			require.NoError(t, err)
			require.Contains(t, string(data), "/var/log/supervisord/root-supervisord-shim")
			require.FileExists(t, filepath.Join(services, "supervisord-shim.conf"))
			for path, mode := range map[string]os.FileMode{"root.conf": 0o600, "root-services": 0o700, "supervisord.conf": 0o644} {
				stat, err := os.Stat(filepath.Join(dir, path))
				require.NoError(t, err)
				require.Equal(t, mode, stat.Mode().Perm())
			}

			// Customer configuration must not be promoted or followed by root on reboot.
			rootDbus, err := os.ReadFile(filepath.Join(dir, "root-services/dbus.conf"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(services, "dbus.conf"), []byte("untrusted"), 0o644))
			protected := filepath.Join(dir, "protected")
			require.NoError(t, os.WriteFile(protected, []byte("unchanged"), 0o600))
			require.NoError(t, os.Remove(filepath.Join(services, "chromium.conf")))
			require.NoError(t, os.Symlink(protected, filepath.Join(services, "chromium.conf")))
			require.NoError(t, splitSupervisorServices(dir, logs, os.Getuid(), os.Getgid()))
			data, err = os.ReadFile(filepath.Join(dir, "root-services/dbus.conf"))
			require.NoError(t, err)
			require.Equal(t, rootDbus, data)
			data, err = os.ReadFile(protected)
			require.NoError(t, err)
			require.Equal(t, "unchanged", string(data))
		})
	}
}

func TestSocketReadinessRejectsStaleSockets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.True(t, unixSocketReady(path))
	require.NoError(t, listener.Close())
	require.FileExists(t, path)
	require.False(t, unixSocketReady(path))
}

func TestLogReaderDropsPrivileges(t *testing.T) {
	t.Setenv("RUN_AS_ROOT", "false")
	cmd := tailCommand("/var/log/supervisord/chromium")
	require.Contains(t, cmd.Args, "--reuid=kernel")
	require.Contains(t, cmd.Args, "--no-new-privs")
	t.Setenv("RUN_AS_ROOT", "true")
	require.Equal(t, "tail", filepath.Base(tailCommand("/test").Path))
}

func TestUserSupervisorCannotAcquirePrivileges(t *testing.T) {
	cmd := userSupervisorCommand()
	require.Contains(t, cmd.Args, "--no-new-privs")
	require.Contains(t, cmd.Args, "--reuid=kernel")
	require.Contains(t, cmd.Args, "--regid=kernel")
	require.Contains(t, cmd.Env, "HOME=/home/kernel")
}
