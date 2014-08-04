package autoupdate

/*
Update sets are sets of packages generated by Gemnasium, aim to be test
in projects to determine if updates are going to pass.
These functions are meant to be used during CI tests.
*/

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gemnasium/toolbelt/config"
	"github.com/gemnasium/toolbelt/gemnasium"
	"github.com/gemnasium/toolbelt/models"
	"github.com/gemnasium/toolbelt/utils"
)

const (
	AUTOUPDATE_MAX_DURATION = 3600
	UPDATE_SET_INVALID      = "invalid"
	UPDATE_SET_SUCCESS      = "test_passed"
	UPDATE_SET_FAIL         = "test_failed"
)

type RequirementUpdate struct {
	File  models.DependencyFile `json:"file"`
	Patch string                `json:"patch"`
}

type VersionUpdate struct {
	Package       models.Package
	OldVersion    string `json:"old_version"`
	TargetVersion string `json:"target_version"`
}

type UpdateSet struct {
	ID                 int                            `json:"id"`
	RequirementUpdates map[string][]RequirementUpdate `json:"requirement_updates"`
	VersionUpdates     map[string][]VersionUpdate     `json:"version_updates"`
}

type UpdateSetResult struct {
	UpdateSetID     int                     `json:"-"`
	ProjectSlug     string                  `json:"-"`
	State           string                  `json:"state"`
	DependencyFiles []models.DependencyFile `json:"dependency_files"`
}

// Download and loop over update sets, apply changes, run test suite, and finally notify gemnasium
func Run(projectSlug string, testSuite []string) error {
	if envTS := os.Getenv(config.ENV_GEMNASIUM_TESTSUITE); envTS != "" {
		testSuite = strings.Fields(envTS)
	}
	if len(testSuite) == 0 {
		return errors.New("Arg [testSuite] can't be empty")
	}

	fmt.Printf("Executing test script: ")
	out, err := executeTestSuite(testSuite)
	if err != nil {
		fmt.Println("Aborting, initial test suite run is failing:")
		fmt.Printf("%s\n", out)
		return err
	}

	// We'll be checking loop duration on each iteration
	startTime := time.Now()
	// Loop until tests are green
	for {
		if time.Since(startTime).Seconds() > AUTOUPDATE_MAX_DURATION {
			fmt.Println("Max loop duration reached, aborting.")
			break
		}
		updateSet, err := fetchUpdateSet(projectSlug)
		if err != nil {
			if err.Error() == "Server returned non-200 status: 409 Conflict\n" {
				fmt.Printf("The current revision (%s) is unknown on Gemnasium, please push your dependency files before running autoupdate.\nSee `gemnasium df help push`.\n", utils.GetCurrentRevision())
			}
			return err
		}
		if updateSet.ID == 0 {
			fmt.Println("Job done!")
			break
		}
		fmt.Printf("\n========= [UpdateSet #%d] =========\n", updateSet.ID)

		// We have an updateSet, let's patch files and run tests
		// We need to keep a list of updated files to restore them after this run
		orgDepFiles, uptDepFiles, err := applyUpdateSet(updateSet)
		resultSet := &UpdateSetResult{UpdateSetID: updateSet.ID, ProjectSlug: projectSlug, DependencyFiles: uptDepFiles}
		if err == cantInstallRequirements || err == cantUpdateVersions {
			resultSet.State = UPDATE_SET_INVALID
			err := pushUpdateSetResult(resultSet)
			if err != nil {
				return err
			}

			err = restoreDepFiles(orgDepFiles)
			if err != nil {
				fmt.Printf("Error while restoring files: %s\n", err)
			}
			// No need to try the update, it will fail
			continue
		}
		if err != nil {
			return err
		}

		out, err := executeTestSuite(testSuite)
		if err == nil {
			// we found a valid candidate
			resultSet.State = UPDATE_SET_SUCCESS
			err := pushUpdateSetResult(resultSet)
			if err != nil {
				return err
			}

			err = restoreDepFiles(orgDepFiles)
			if err != nil {
				return err
			}

			continue
		}
		// display cmd output
		fmt.Printf("%s\n", out)
		resultSet.State = UPDATE_SET_FAIL
		err = pushUpdateSetResult(resultSet)
		if err != nil {
			return err
		}
		err = restoreDepFiles(orgDepFiles)
		if err != nil {
			fmt.Printf("Error while restoring files: %s\n", err)
		}
		// Let's continue with another set
	}
	return nil
}

func fetchUpdateSet(projectSlug string) (*UpdateSet, error) {
	revision := utils.GetCurrentRevision()
	if revision == "" {
		return nil, errors.New("Can't determine current revision, please use REVISION env var to specify it")
	}
	var updateSet *UpdateSet
	opts := &gemnasium.APIRequestOptions{
		Method: "POST",
		URI:    fmt.Sprintf("/projects/%s/branches/%s/update_sets/next", projectSlug, utils.GetCurrentBranch()),
		Body:   &map[string]string{"revision": revision},
		Result: &updateSet,
	}
	err := gemnasium.APIRequest(opts)
	if err != nil {
		return nil, err
	}

	return updateSet, nil
}

// Patch files if needed, and update packages
// Will return a slice of original files and a slice of the updated files, with
// their content
func applyUpdateSet(updateSet *UpdateSet) (orgDepFiles, uptDepFiles []models.DependencyFile, err error) {
	for packageType, reqUpdates := range updateSet.RequirementUpdates {
		installer, err := NewRequirementsInstaller(packageType)
		if err != nil {
			return orgDepFiles, uptDepFiles, err
		}

		err = installer(reqUpdates, &orgDepFiles, &uptDepFiles)
		if err != nil {
			return orgDepFiles, uptDepFiles, err
		}
	}

	for packageType, versionUpdates := range updateSet.VersionUpdates {
		// Update Versions
		updater, err := NewUpdater(packageType)
		if err != nil {
			return orgDepFiles, uptDepFiles, err
		}
		err = updater(versionUpdates, &orgDepFiles, &uptDepFiles)
		if err != nil {
			return orgDepFiles, uptDepFiles, err
		}
	}
	fmt.Println("Done")
	return orgDepFiles, uptDepFiles, nil
}

// Once update set has been tested, we must send the result to Gemnasium,
// in order to update statitics.
func pushUpdateSetResult(rs *UpdateSetResult) error {
	fmt.Printf("Pushing result (status='%s'): ", rs.State)

	if rs.UpdateSetID == 0 || rs.State == "" {
		return errors.New("Missing updateSet ID and/or State args")
	}

	opts := &gemnasium.APIRequestOptions{
		Method: "PATCH",
		URI:    fmt.Sprintf("/projects/%s/branches/%s/update_sets/%d", rs.ProjectSlug, utils.GetCurrentBranch(), rs.UpdateSetID),
		Body:   rs,
	}
	err := gemnasium.APIRequest(opts)
	if err != nil {
		return err
	}

	fmt.Printf("done\n")
	return nil
}

// Restore original files.
// Needed after each run
func restoreDepFiles(dfiles []models.DependencyFile) error {
	fmt.Printf("%d file(s) to be restored.\n", len(dfiles))
	for _, df := range dfiles {
		fmt.Printf("Restoring file %s: ", df.Path)
		err := ioutil.WriteFile(df.Path, df.Content, 0644)
		if err != nil {
			return err
		}
		fmt.Printf("done\n")
	}
	return nil
}

func executeTestSuite(ts []string) ([]byte, error) {
	type Result struct {
		Output []byte
		Err    error
	}
	done := make(chan Result)
	defer close(done)
	var out []byte
	var err error
	fmt.Printf("Executing test script")
	start := time.Now()
	go func() {
		result, err := exec.Command(ts[0], ts[1:]...).Output()
		done <- Result{result, err}
	}()
	var stop bool
	for {
		select {
		case result := <-done:
			stop = true
			out = result.Output
			err = result.Err
		default:
			fmt.Print(".")
			time.Sleep(1 * time.Second)
		}
		if stop {
			break
		}
	}
	fmt.Printf("done (%fs)\n", time.Since(start).Seconds())
	return out, err
}
