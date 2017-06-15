package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/vcs"
	"github.com/deis/acid/pkg/js"
	"github.com/google/go-github/github"

	"gopkg.in/gin-gonic/gin.v1"
)

const (
	GitHubEvent  = `X-GitHub-Event`
	HubSignature = `X-Hub-Signature`
)

const (
	runnerJS = "runner.js"
	acidJS   = "acid.js"
)

// EventRouter routes a webhook to its appropriate handler.
//
// It does this by sniffing the event from the header, and routing accordingly.
func EventRouter(c *gin.Context) {
	event := c.Request.Header.Get(GitHubEvent)
	switch event {
	case "":
		// TODO: Once we're wired up with GitHub, need to return here.
		log.Print("No event header.")
		c.JSON(200, gin.H{"message": "OK"})
		return
	case "ping":
		log.Print("Received ping from GitHub")
		c.JSON(200, gin.H{"message": "OK"})
		return
	case "push":
		Push(c)
		return
	default:
		log.Printf("Expected event push, got %s", event)
		c.JSON(http.StatusBadRequest, gin.H{"status": "Only 'push' is supported. Got " + event})
		return
	}
}

// Push responds to a push event.
func Push(c *gin.Context) {
	// Only process push for now. Other hooks have different formats.
	signature := c.Request.Header.Get(HubSignature)

	body, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("Failed to read body: %s", err)
		c.JSON(http.StatusBadRequest, gin.H{"status": "Malformed body"})
		return
	}
	defer c.Request.Body.Close()

	push := &PushHook{}
	if err := json.Unmarshal(body, push); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": err.Error()})
		return
	}

	// Load config and verify data.
	pname := "acid-" + ShortSHA(push.Repository.FullName)
	proj, err := LoadProjectConfig(pname, "default")
	if err != nil {
		log.Printf("Project %q (%q) not found. No secret loaded. %s", push.Repository.FullName, pname, err)
		c.JSON(http.StatusBadRequest, gin.H{"status": "project not found"})
		return
	}

	if proj.Secret == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "No secret is configured for this repo."})
		return
	}

	// Compare the salted digest in the header with our own computing of the
	// body.
	sum := SHA1HMAC([]byte(proj.Secret), body)
	if subtle.ConstantTimeCompare([]byte(sum), []byte(signature)) != 1 {
		log.Printf("Expected signature %q (sum), got %q (hub-signature)", sum, signature)
		//log.Printf("%s", body)
		c.JSON(http.StatusForbidden, gin.H{"status": "malformed signature"})
		return
	}

	if proj.Name != push.Repository.FullName {
		// TODO: Test this. I believe it should error out if these don't match.
		log.Printf("!!!WARNING!!! Expected project secret to have name %q, got %q", push.Repository.FullName, proj.Name)
	}

	go buildStatus(push, proj)

	c.JSON(http.StatusOK, gin.H{"status": "Complete"})
}

// buildStatus runs a build, and sets upstream status accordingly.
func buildStatus(push *PushHook, proj *Project) {
	// If we need an SSH key, set it here
	if proj.SSHKey != "" {
		key, err := ioutil.TempFile("", "")
		if err != nil {
			log.Printf("error creating ssh key cache: %s", err)
			return
		}
		keyfile := key.Name()
		defer os.Remove(keyfile)
		if _, err := key.WriteString(proj.SSHKey); err != nil {
			log.Printf("error writing ssh key cache: %s", err)
			return
		}
		os.Setenv("ACID_REPO_KEY", keyfile)
		defer os.Unsetenv("ACID_REPO_KEY") // purely defensive... not really necessary
	}

	targetURL := "http://localhost:8080" // FIXME
	msg := "Building"
	svc := StatusContext
	status := &github.RepoStatus{
		State:       &StatePending,
		TargetURL:   &targetURL,
		Description: &msg,
		Context:     &svc,
	}
	if err := setRepoStatus(push, proj, status); err != nil {
		// For this one, we just log an error and continue.
		log.Printf("Error setting status to %s: %s", *status.State, err)
	}
	if err := build(push, proj); err != nil {
		log.Printf("Build failed: %s", err)
		msg = err.Error()
		status.State = &StateFailure
		status.Description = &msg
	} else {
		msg = "Acid build passed"
		status.State = &StateSuccess
		status.Description = &msg
	}
	if err := setRepoStatus(push, proj, status); err != nil {
		// For this one, we just log an error and continue.
		log.Printf("After build, error setting status to %s: %s", *status.State, err)
	}
}

func build(push *PushHook, proj *Project) error {
	toDir := filepath.Join("_cache", push.Repository.FullName)
	if err := os.MkdirAll(toDir, 0755); err != nil {
		log.Printf("error making %s: %s", toDir, err)
		return err
	}

	url := push.Repository.CloneURL
	if len(proj.SSHKey) != 0 {
		log.Printf("Switch to SSH URL %s because key is of length %d", push.Repository.SSHURL, len(proj.SSHKey))
		url = push.Repository.SSHURL
	}

	// TODO:
	// - [ ] Remove the cached directory at the end of the build?
	if err := cloneRepo(url, push.HeadCommit.Id, toDir); err != nil {
		log.Printf("error cloning %s to %s: %s", url, toDir, err)
		return err
	}

	// Path to acid file:
	acidPath := filepath.Join(toDir, acidJS)
	acidScript, err := ioutil.ReadFile(acidPath)
	if err != nil {
		return err
	}
	log.Print(string(acidScript))
	sandbox, err := js.New()
	if err != nil {
		return err
	}

	return execScripts(sandbox, push, proj.SSHKey, acidScript)
}

type originalError interface {
	Original() error
	Out() string
}

func logOriginalError(err error) {
	oerr, ok := err.(originalError)
	if ok {
		log.Println(oerr.Original().Error())
		log.Println(oerr.Out())
	}
}

// execScripts prepares the JS runtime and feeds it the objects it needs.
func execScripts(sandbox *js.Sandbox, push *PushHook, sshKey string, acidJS []byte) error {
	// Serialize push record
	pushRecord, err := json.Marshal(push)
	if err != nil {
		return err
	}

	// Configure sandbox
	sandbox.Variable("sshKey", strings.Replace(sshKey, "\n", "$", -1))
	sandbox.Variable("configName", "acid-"+ShortSHA(push.Repository.FullName))
	// TODO: When we add more events, we need to fix this
	sandbox.Variable("eventName", "push")

	// We do this so that the JSON is correctly marshaled by Go and unmarshaled by Otto.
	if err := sandbox.ExecString(`pushRecord = ` + string(pushRecord)); err != nil {
		return fmt.Errorf("failed JS bootstrap: %s", err)
	}

	log.Println("Loading acid.js")

	// Wrap the AcidJS in a function that we can call later.
	acidScript := `var registerEvents = function(events){` + string(acidJS) + `}`
	if err := sandbox.ExecString(acidScript); err != nil {
		return fmt.Errorf("acid.js is not well formed: %s\n%s", err, acidScript)
	}

	log.Println("Loading runner.js")
	if err := sandbox.Preload("js/runner.js"); err != nil {
		return fmt.Errorf("runner.js: %s", err)
	}

	return nil
}

func cloneRepo(url, version, toDir string) error {
	repo, err := vcs.NewRepo(url, toDir)
	if err != nil {
		return err
	}
	if err := repo.Get(); err != nil {
		logOriginalError(err) // FIXME: Audit this in case this might dump sensitive info.
		if err2 := repo.Update(); err2 != nil {
			logOriginalError(err2)
			log.Printf("WARNING: Could neither clone nor update repo %q. Clone: %s Update: %s", url, err, err2)
		}
	}

	if err := repo.UpdateVersion(version); err != nil {
		log.Printf("Failed to checkout %q: %s", version, err)
		return err
	}

	return nil
}
