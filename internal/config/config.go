// Package config 管理 ~/.runlite 下的路径与 settings.json。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Settings struct {
	JdkPaths     map[string]string `json:"toolPaths,omitempty"` // java/mvn/gradle/node -> 可执行文件路径
	StartTimeout string            `json:"startTimeout,omitempty"`
	StopGrace    string            `json:"stopGrace,omitempty"`
	RingSize     int               `json:"ringSize,omitempty"`
	AutoPrune    *bool             `json:"autoPrune,omitempty"`
}

type Paths struct {
	Home     string
	Projects string
	Runs     string
	Settings string
	Daemon   string
}

func DefaultHome() string {
	if h := os.Getenv("RUNLITE_HOME"); h != "" {
		return h
	}
	base, err := os.UserHomeDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, ".runlite")
}

func For(home string) *Paths {
	return &Paths{
		Home:     home,
		Projects: filepath.Join(home, "projects.json"),
		Runs:     filepath.Join(home, "runs"),
		Settings: filepath.Join(home, "settings.json"),
		Daemon:   filepath.Join(home, "daemon.json"),
	}
}

func (p *Paths) Ensure() error { return os.MkdirAll(p.Home, 0o700) }

func LoadSettings(path string) (*Settings, error) {
	s := &Settings{RingSize: 2000}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return s, nil
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	if s.RingSize <= 0 {
		s.RingSize = 2000
	}
	return s, nil
}

type DaemonInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Exe     string `json:"exe"`
	Started string `json:"started"`
	// Created 是 daemon 进程的创建时间（Windows FILETIME）。PID 会被系统回收复用，
	// 客户端要靠它区分"记录里那个 daemon 还在启动中"和"这个号码已经换人了"。
	Created uint64 `json:"created,omitempty"`
}

func ReadDaemonInfo(path string) (*DaemonInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d DaemonInfo
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Port <= 0 || d.Token == "" {
		return nil, os.ErrInvalid
	}
	return &d, nil
}

func WriteDaemonInfo(path string, d *DaemonInfo) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
