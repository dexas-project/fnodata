package main

import (
	"flag"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/fonero-project/fnod/fnoutil"
)

var tempConfigFile *os.File
var tempAppDataDir string

func TestMain(m *testing.M) {
	// Temp config file is used to ensure there are no external influences
	// from previously set env variables or default config files.
	tempConfigFile, _ = ioutil.TempFile("", "fnodata_test_file.cfg")
	defer os.Remove(tempConfigFile.Name())
	os.Setenv("FNODATA_CONFIG_FILE", tempConfigFile.Name())

	// Make an empty folder for appdata tests.
	tempAppDataDir, _ = ioutil.TempDir("", "fnodata_test_appdata")
	defer os.RemoveAll(tempAppDataDir)

	// Parse the -test.* flags before removing them from the command line
	// arguments list, which we do to allow go-flags to succeed.
	flag.Parse()
	os.Args = os.Args[:1]
	// Run the tests now that the testing package flags have been parsed.
	m.Run()
	os.Unsetenv("FNODATA_CONFIG_FILE")
}

// disableConfigFileEnv checks if the FNODATA_CONFIG_FILE environment variable
// is set, unsets it, and returns a function that will return
// FNODATA_CONFIG_FILE to its state before calling disableConfigFileEnv.
func disableConfigFileEnv() func() {
	loc, wasSet := os.LookupEnv("FNODATA_CONFIG_FILE")
	if wasSet {
		os.Unsetenv("FNODATA_CONFIG_FILE")
		return func() { os.Setenv("FNODATA_CONFIG_FILE", loc) }
	}
	return func() {}
}

func TestLoadCustomConfigPresent(t *testing.T) {
	// Load using the empty config file set via environment variable in
	// TestMain. Since the file exists, it should not cause an error.
	_, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}
}

func TestLoadDefaultConfigMissing(t *testing.T) {
	// Unset the custom config file.
	restoreConfigFileLoc := disableConfigFileEnv()
	defer restoreConfigFileLoc()

	// Use the empty appdata dir.
	os.Setenv("FNODATA_APPDATA_DIR", tempAppDataDir)
	defer os.Unsetenv("FNODATA_APPDATA_DIR")

	// Load using the the empty appdata directory (with no config file). Since
	// this is the default config file, it should not cause an error.
	_, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}
}

func TestLoadCustomConfigMissing(t *testing.T) {
	// Unset the custom config file.
	restoreConfigFileLoc := disableConfigFileEnv()
	defer restoreConfigFileLoc()

	// Set a path to a non-existent config file. Use TempFile followed by Remove
	// to guarantee the file does not exist.
	goneFile, _ := ioutil.TempFile("", "blah")
	os.Remove(goneFile.Name())
	os.Setenv("FNODATA_CONFIG_FILE", goneFile.Name())

	// Attempt to load using the non-existent non-default config file, which
	// should return an error.
	_, err := loadConfig()
	if err == nil {
		t.Errorf("Loaded fnodata config, but the explicitly set config file"+
			"%s does not exist.", goneFile.Name())
	}
}

// TestLoadDefaultConfigPathCustomAppdata ensures that setting appdata while the
// config file is not explicitly set will change the default config file
// location, and that there is no error if this new default config file does not
// exist as missing config files are only an error when explicitly set.
func TestLoadDefaultConfigPathCustomAppdata(t *testing.T) {
	// Unset the custom config file.
	restoreConfigFileLoc := disableConfigFileEnv()
	defer restoreConfigFileLoc()

	// Use the empty appdata dir.
	os.Setenv("FNODATA_APPDATA_DIR", tempAppDataDir)
	defer os.Unsetenv("FNODATA_APPDATA_DIR")

	// Load using the the empty appdata directory (with no config file). Since
	// this is the default config file, it should not cause an error.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	// Verify that the default config file is located in the specified appdata
	// directory rather than the default appdata directory.
	expected := filepath.Join(tempAppDataDir, defaultConfigFilename)
	if cfg.ConfigFile != expected {
		t.Errorf("Default config file expected at %s, got %s", expected, cfg.ConfigFile)
	}
}

func TestDefaultConfigAPIListen(t *testing.T) {
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.APIListen != defaultAPIListen {
		t.Errorf("Expected API listen URL %s, got %s", defaultAPIListen, cfg.APIListen)
	}
}

func TestDefaultConfigAPIListenWithEnv(t *testing.T) {
	customListenPath := "0.0.0.0:7777"
	os.Setenv("FNODATA_LISTEN_URL", customListenPath)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.APIListen != customListenPath {
		t.Errorf("Expected API listen URL %s, got %s", customListenPath, cfg.APIListen)
	}
}

func TestDefaultConfigAppDataDir(t *testing.T) {
	expected := fnoutil.AppDataDir("fnodata", false)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.HomeDir != expected {
		t.Errorf("Expected appdata directory %s, got %s", expected, cfg.HomeDir)
	}
}

func TestCustomHomeDirWithEnv(t *testing.T) {
	// Do not override config file as appdata changes its location.
	restoreConfigFileLoc := disableConfigFileEnv()
	defer restoreConfigFileLoc()

	// Use the empty appdata dir made for the tests.
	os.Setenv("FNODATA_APPDATA_DIR", tempAppDataDir)
	defer os.Unsetenv("FNODATA_APPDATA_DIR")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.HomeDir != tempAppDataDir {
		t.Errorf("Expected appdata directory %s, got %s", tempAppDataDir, cfg.HomeDir)
	}
}

// Ensure that command line flags override env variables.
func TestDefaultConfigHomeDirWithEnvAndFlag(t *testing.T) {
	tmp2 := "fnodata_test_appdata2"
	cliOverride, err := ioutil.TempDir("", tmp2)
	if err != nil {
		t.Fatalf("Unable to create temporary folder %s: %v", tmp2, err)
	}
	defer os.RemoveAll(cliOverride)
	os.Args = append(os.Args, "--appdata="+cliOverride)

	os.Setenv("FNODATA_APPDATA_DIR", cliOverride)
	defer os.Unsetenv("FNODATA_APPDATA_DIR")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.HomeDir != cliOverride {
		t.Errorf("Expected appdata directory %s, got %s", cliOverride, cfg.HomeDir)
	}
}

func TestDefaultConfigNetwork(t *testing.T) {
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}

	if cfg.TestNet || cfg.SimNet {
		t.Errorf("Default config should be for mainnet but was not.")
	}
}

func TestDefaultConfigTestNetWithEnv(t *testing.T) {
	os.Setenv("FNODATA_USE_TESTNET", "true")
	defer os.Unsetenv("FNODATA_USE_TESTNET")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load fnodata config: %v", err)
	}
	if !cfg.TestNet {
		t.Errorf("Testnet was specified via environment variable, but not using testnet.")
	}
}

func TestDefaultConfigTestNetWithEnvAndBadValue(t *testing.T) {
	os.Setenv("FNODATA_USE_TESTNET", "no")
	defer os.Unsetenv("FNODATA_USE_TESTNET")

	_, err := loadConfig()
	if err == nil {
		t.Errorf("Invalid boolean value for FNODATA_USE_TESTNET did not cause an error.")
	}
}
