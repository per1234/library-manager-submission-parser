// Copyright 2021 ARDUINO SA (http://www.arduino.cc/)
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/sourcegraph/go-diff/diff"

	"github.com/arduino/go-paths-helper"
	properties "github.com/arduino/go-properties-orderedmap"
)

// Git hosts that are supported for library repositories.
var supportedHosts []string = []string{
	"bitbucket.org",
	"git.antares.id",
	"github.com",
	"gitlab.com",
}

// Libraries under these organizations will have the "Arduino" type and be linted with Arduino Lint in the "official" setting.
var officialOrganizations []string = []string{
	"github.com/arduino",
	"github.com/arduino-libraries",
	"github.com/bcmi-labs",
	"github.com/vidor-libraries",
}

// Libraries under these organizations will have the "Partner" type.
var partnerOrganizations []string = []string{
	"github.com/Azure",
	"github.com/ms-iot",
	"github.com/ameltech",
}

// Libraries under these organizations will have the "Recommended" type.
var recommendedOrganizations []string = []string{
	"github.com/adafruit",
}

// request is the type of the request data.
type request struct {
	Type                             string           `json:"type"`                             // Request type.
	ArduinoLintLibraryManagerSetting string           `json:"arduinoLintLibraryManagerSetting"` // Argument to pass to Arduino Lint's --library-manager flag.
	Submissions                      []submissionType `json:"submissions"`                      // Data for submitted libraries.
	IndexEntry                       string           `json:"indexEntry"`                       // Entry that will be made to the Library Manager index source file when the submission is accepted.
	IndexerLogsURLs                  string           `json:"indexerLogsURLs"`                  // List of URLs where the logs from the Library Manager indexer for each submission are available for view.
	Error                            string           `json:"error"`                            // Error message.
}

// submissionType is the type of the data for each individual library submitted in the request.
type submissionType struct {
	SubmissionURL  string `json:"submissionURL"`  // Library repository URL as submitted by user. Used to identify the submission to the user.
	NormalizedURL  string `json:"normalizedURL"`  // Submission URL in the standardized format that will be used in the index entry.
	RepositoryName string `json:"repositoryName"` // Name of the submission's repository.
	Name           string `json:"name"`           // Library name.
	Official       bool   `json:"official"`       // Whether the library is official.
	Tag            string `json:"tag"`            // Name of the submission repository's latest tag, which is used as the basis for the index entry and validation.
	Error          string `json:"error"`          // Error message.
}

// Command line flags.
var diffPathArgument = flag.String("diffpath", "", "")
var repoPathArgument = flag.String("repopath", "", "")
var listNameArgument = flag.String("listname", "", "")

func main() {
	// Validate flag input.
	flag.Parse()

	if *diffPathArgument == "" {
		errorExit("--diffpath flag is required")
	}

	if *repoPathArgument == "" {
		errorExit("--repopath flag is required")
	}

	if *listNameArgument == "" {
		errorExit("--listname flag is required")
	}

	diffPath := paths.New(*diffPathArgument)
	exist, err := diffPath.ExistCheck()
	if !exist {
		errorExit("diff file not found")
	}

	listPath := paths.New(*repoPathArgument, *listNameArgument)
	exist, err = listPath.ExistCheck()
	if !exist {
		errorExit(fmt.Sprintf("list file %s not found", listPath))
	}

	// Parse the PR diff.
	rawDiff, err := diffPath.ReadFile()
	if err != nil {
		panic(err)
	}
	var req request
	var submissionURLs []string
	req.Type, req.Error, req.ArduinoLintLibraryManagerSetting, submissionURLs = parseDiff(rawDiff, *listNameArgument)

	// Process the submissions.
	var indexEntries []string
	var indexerLogsURLs []string
	for _, submissionURL := range submissionURLs {
		submission, indexEntry := populateSubmission(submissionURL, listPath)
		req.Submissions = append(req.Submissions, submission)
		indexEntries = append(indexEntries, indexEntry)
		indexerLogsURLs = append(indexerLogsURLs, indexerLogsURL(submission.NormalizedURL))
	}

	// Check for duplicates within the submission itself.
	submissionURLMap := make(map[string]bool)
	for submissionIndex, submission := range req.Submissions {
		submissionURLMap[submission.NormalizedURL] = true
		if len(submissionURLMap) <= submissionIndex {
			req.Submissions[submissionIndex].Error = "Submission contains duplicate URLs."
		}
	}

	// Assemble the index entry for the submissions.
	// Note: %0A must be used for line breaks in all strings that will be used as step/job outputs in the GitHub Actions
	// workflow. In that application, any text following \n is discarded.
	req.IndexEntry = strings.Join(indexEntries, "%0A")

	// Assemble the list of Library Manager indexer logs URLs for the submissions to show in the acceptance message.
	req.IndexerLogsURLs = strings.Join(indexerLogsURLs, "%0A")

	// Marshal the request data into a JSON document.
	var marshalledRequest bytes.Buffer
	jsonEncoder := json.NewEncoder(io.Writer(&marshalledRequest))
	// By default, the json package HTML-sanitizes strings during marshalling (https://golang.org/pkg/encoding/json/#Marshal)
	// It's not possible to change this behavior when using the simple json.MarshalIndent() approach.
	jsonEncoder.SetEscapeHTML(false)
	jsonEncoder.SetIndent("", "") // Single line.
	err = jsonEncoder.Encode(req)
	if err != nil {
		panic(err)
	}

	fmt.Println(marshalledRequest.String())
}

// errorExit prints the error message in a standardized format and exits with status 1.
func errorExit(message string) {
	fmt.Printf("ERROR: %s\n", message)
	os.Exit(1)
}

// parseDiff parses the request diff and returns the request type, request error, `arduino-lint --library-manager` setting, and list of submission URLs.
func parseDiff(rawDiff []byte, listName string) (string, string, string, []string) {
	var submissionURLs []string

	// Check if the PR has removed the final newline from a file, which would cause a spurious diff for the next PR if merged.
	// Unfortunately, the diff package does not have this capability (only to detect missing newline in the original file).
	if bytes.Contains(rawDiff, []byte("\\ No newline at end of file")) {
		return "invalid", "Pull request removes newline from the end of a file.%0APlease add a blank line to the end of the file.", "", nil
	}

	diffs, err := diff.ParseMultiFileDiff(rawDiff)
	if err != nil {
		panic(err)
	}

	if (len(diffs) != 1) || (diffs[0].OrigName[2:] != listName) || (diffs[0].OrigName[2:] != diffs[0].NewName[2:]) { // Git diffs have a a/ or b/ prefix on file names.
		// This is not a Library Manager submission.
		return "other", "", "", nil
	}

	var addedCount int
	var deletedCount int
	// Get the added URLs from the diff
	for _, hunk := range diffs[0].Hunks {
		hunkBody := string(hunk.Body)
		for _, rawDiffLine := range strings.Split(hunkBody, "\n") {
			diffLine := strings.TrimRight(rawDiffLine, " \t")
			if len(diffLine) < 2 {
				continue // Ignore blank lines.
			}

			switch diffLine[0] {
			case '+':
				addedCount++
				submissionURLs = append(submissionURLs, strings.TrimSpace(diffLine[1:]))
			case '-':
				deletedCount++
			default:
				continue
			}
		}
	}

	var requestType string
	var arduinoLintLibraryManagerSetting string
	if addedCount == 0 && deletedCount == 0 {
		requestType = "other"
	} else if addedCount > 0 && deletedCount == 0 {
		requestType = "submission"
		arduinoLintLibraryManagerSetting = "submit"
	} else if addedCount == 0 && deletedCount > 0 {
		requestType = "removal"
		arduinoLintLibraryManagerSetting = ""
	} else {
		requestType = "modification"
		arduinoLintLibraryManagerSetting = "update"
	}

	return requestType, "", arduinoLintLibraryManagerSetting, submissionURLs
}

// populateSubmission does the checks on the submission that aren't provided by Arduino Lint and gathers the necessary data on it.
func populateSubmission(submissionURL string, listPath *paths.Path) (submissionType, string) {
	indexSourceSeparator := "|"
	var submission submissionType

	submission.SubmissionURL = submissionURL

	// Normalize and validate submission URL.
	submissionURLObject, err := url.Parse(submission.SubmissionURL)
	if err != nil {
		submission.Error = fmt.Sprintf("Invalid submission URL (%s)", err)
		return submission, ""
	}

	// Check if URL is accessible.
	httpResponse, err := http.Get(submissionURLObject.String())
	if err != nil {
		submission.Error = fmt.Sprintf("Unable to load submission URL: %s", err)
		return submission, ""
	}
	if httpResponse.StatusCode != http.StatusOK {
		submission.Error = "Unable to load submission URL. Is the repository public?"
		return submission, ""
	}

	// Resolve redirects and normalize.
	normalizedURLObject := normalizeURL(httpResponse.Request.URL)

	submission.NormalizedURL = normalizedURLObject.String()

	// Check if URL is from a supported Git host.
	if !uRLIsUnder(normalizedURLObject, supportedHosts) {
		submission.Error = fmt.Sprintf("`%s` is not currently supported as a Git hosting website for Library Manager.%%0A%%0ASee: https://github.com/arduino/library-registry/blob/main/FAQ.md#what-are-the-requirements-for-a-library-to-be-added-to-library-manager", normalizedURLObject.Host)
		return submission, ""
	}

	// Check if URL is a Git repository
	err = exec.Command("git", "ls-remote", normalizedURLObject.String()).Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			submission.Error = "Submission URL is not a Git clone URL (e.g., `https://github.com/arduino-libraries/Servo`)."
			return submission, ""
		}

		panic(err)
	}

	submission.RepositoryName = strings.TrimSuffix(paths.New(normalizedURLObject.Path).Base(), ".git")

	// Check if the URL is already in the index.
	listLines, err := listPath.ReadFileAsLines()
	for _, listURL := range listLines {
		listURLObject, err := url.Parse(strings.TrimSpace(listURL))
		if err != nil {
			panic(err) // All list items have already passed parsing so something is broken if this happens.
		}

		normalizedListURLObject := normalizeURL(listURLObject)
		if normalizedListURLObject.String() == normalizedURLObject.String() {
			submission.Error = "Submission URL is already in the Library Manager index."
			return submission, ""
		}
	}

	// Determine the library types attributes.
	submission.Official = uRLIsUnder(normalizedURLObject, officialOrganizations)
	var types []string
	if submission.Official {
		types = append(types, "Arduino")
	}
	if uRLIsUnder(normalizedURLObject, partnerOrganizations) {
		types = append(types, "Partner")
	}
	if uRLIsUnder(normalizedURLObject, recommendedOrganizations) {
		types = append(types, "Recommended")
	}
	if types == nil {
		types = append(types, "Contributed")
	}

	submissionClonePath, err := paths.MkTempDir("", "")
	if err != nil {
		panic(err)
	}

	err = exec.Command("git", "clone", "--depth", "1", normalizedURLObject.String(), submissionClonePath.String()).Run()
	if err != nil {
		panic(err)
	}

	// Determine latest tag name in submission repo
	err = os.Chdir(submissionClonePath.String())
	if err != nil {
		panic(err)
	}
	err = exec.Command("git", "fetch", "--tags").Run()
	if err != nil {
		panic(err)
	}
	tagList, err := exec.Command("git", "rev-list", "--tags", "--max-count=1").Output()
	if err != nil {
		panic(err)
	}
	if string(tagList) == "" {
		submission.Error = "The repository has no tags. You need to create a [release](https://docs.github.com/en/github/administering-a-repository/managing-releases-in-a-repository) or [tag](https://git-scm.com/docs/git-tag) that matches the `version` value in the library's library.properties file."
		return submission, ""
	}
	latestTag, err := exec.Command("git", "describe", "--tags", strings.TrimSpace(string(tagList))).Output()
	if err != nil {
		panic(err)
	}
	submission.Tag = strings.TrimSpace(string(latestTag))

	// Checkout latest tag.
	err = exec.Command("git", "checkout", submission.Tag).Run()
	if err != nil {
		panic(err)
	}

	// Get submission library name. It is necessary to record this in the index source entry because the library is locked to this name.
	libraryPropertiesPath := submissionClonePath.Join("library.properties")
	if !libraryPropertiesPath.Exist() {
		submission.Error = "Library is missing a library.properties metadata file.%0A%0ASee: https://arduino.github.io/arduino-cli/latest/library-specification/#library-metadata"
		return submission, ""
	}
	libraryProperties, err := properties.LoadFromPath(libraryPropertiesPath)
	if err != nil {
		submission.Error = fmt.Sprintf("Invalid library.properties file: %s%%0A%%0ASee: https://arduino.github.io/arduino-cli/latest/library-specification/#library-metadata", err)
		return submission, ""
	}
	var ok bool
	submission.Name, ok = libraryProperties.GetOk("name")
	if !ok {
		submission.Error = "library.properties is missing a name field.%0A%0ASee: https://arduino.github.io/arduino-cli/latest/library-specification/#library-metadata"
		return submission, ""
	}

	// Assemble Library Manager index source entry string
	indexEntry := strings.Join(
		[]string{
			submission.NormalizedURL,
			strings.Join(types, ","),
			submission.Name,
		},
		indexSourceSeparator,
	)

	return submission, indexEntry
}

// normalizeURL converts the URL into the standardized format used in the index.
func normalizeURL(rawURL *url.URL) url.URL {
	normalizedPath := strings.TrimRight(rawURL.Path, "/")
	if normalizedPath == "" {
		// It doesn't make sense to add the extension to root URLs
		normalizedPath = "/"
	} else if !strings.HasSuffix(normalizedPath, ".git") {
		normalizedPath += ".git"
	}

	return url.URL{
		Scheme: "https",
		Host:   rawURL.Host,
		Path:   normalizedPath,
	}
}

// indexerLogsURL returns the URL where the logs from the Library Manager indexer are available for view.
func indexerLogsURL(normalizedURL string) string {
	normalizedURLObject, err := url.Parse(normalizedURL)
	if err != nil {
		panic(err)
	}

	indexerLogsURLObject := url.URL{
		Scheme: "http",
		Host:   "downloads.arduino.cc",
		Path:   "/libraries/logs/" + normalizedURLObject.Host + strings.TrimSuffix(normalizedURLObject.Path, ".git") + "/",
	}

	return indexerLogsURLObject.String()
}

func uRLIsUnder(childURL url.URL, parentCandidates []string) bool {
	for _, parentCandidate := range parentCandidates {
		if !strings.HasSuffix(parentCandidate, "/") {
			parentCandidate += "/"
		}
		parentCandidateURL, err := url.Parse("https://" + parentCandidate)
		if err != nil {
			panic(err)
		}

		childURLPath := paths.New(childURL.Path)
		candidateURLPath := paths.New(parentCandidateURL.Path)

		isUnderPath, err := childURLPath.IsInsideDir(candidateURLPath)
		if err != nil {
			panic(err)
		}

		if (childURL.Host == parentCandidateURL.Host) && (childURLPath.EqualsTo(candidateURLPath) || isUnderPath) {
			return true
		}
	}

	return false
}
