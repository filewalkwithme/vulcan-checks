package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/parser"
	"github.com/microcosm-cc/bluemonday"

	check "github.com/adevinta/vulcan-check-sdk"
	"github.com/adevinta/vulcan-check-sdk/state"
	report "github.com/adevinta/vulcan-report"
)

var (
	checkName = "vulcan-snyk"
	logger    = check.NewCheckLog(checkName)
)

func main() {
	run := func(ctx context.Context, target string, optJSON string, state state.State) (err error) {
		options, err := parseOptions(optJSON)
		if err != nil {
			return err
		}

		organizationName, repositoryName, err := parseTarget(target)
		if err != nil {
			return err
		}

		snykOrgID := options.OrganizationNameToSnykID[*organizationName]

		snykProjects, err := getProjects(snykOrgID)
		if err != nil {
			return err
		}

		r := SnykResponse{}
		for _, snykProject := range snykProjects.Projects {
			snykRepositoryName := getSnykRepositoryName(snykProject, *options)

			if snykRepositoryName == *repositoryName {
				p, err := getProjectIssues(snykOrgID, snykProject.ID)
				if err != nil {
					return err
				}

				for _, v := range p.Issues.Vulnerabilities {
					r.Vulnerabilities = append(r.Vulnerabilities, v)
				}
			}
		}

		// Group vulnerabilities by their Snyk Reference ID and module name, also filter out license issues
		snykVulnerabilitiesMap := make(map[string]map[string][]SnykVulnerability)
		for _, v := range r.Vulnerabilities {
			if v.Type == "license" {
				continue
			}

			_, ok := snykVulnerabilitiesMap[v.ID]
			if !ok {
				snykVulnerabilitiesMap[v.ID] = make(map[string][]SnykVulnerability)
			}

			snykVulnerabilitiesMap[v.ID][v.PackageName] = append(snykVulnerabilitiesMap[v.ID][v.PackageName], v)
		}

		vulns := []report.Vulnerability{}
		// for each pair of (snyk vuln & module name), create a vulcan vuln
		for _, snykModulesMap := range snykVulnerabilitiesMap {
			for moduleName, snykIssues := range snykModulesMap {
				vulcanVulnerability := &report.Vulnerability{}

				vulcanVulnerability.Summary = snykIssues[0].Title + ": " + moduleName
				vulcanVulnerability.Description = extractOverview([]byte(snykIssues[0].Description))
				vulcanVulnerability.Details = createDetails(snykIssues)
				vulcanVulnerability.Score = snykIssues[0].CVSSScore
				vulcanVulnerability.Recommendations = extractRecommendations([]byte(snykIssues[0].Description))

				cweCount := len(snykIssues[0].Identifiers.CWE)
				if cweCount > 0 {
					if cweCount > 1 {
						logger.Infof("Multiple CWE found (SNYK ID: %f). Storing the first CWE found.", snykIssues[0].CVSSScore)
					}

					cweID, err := strconv.Atoi(strings.ReplaceAll(snykIssues[0].Identifiers.CWE[0], "CWE-", ""))
					if err != nil {
						logger.Errorf("Not possible to convert %s to uint32", snykIssues[0].Identifiers.CWE[0])
					} else {
						vulcanVulnerability.CWEID = uint32(cweID)
					}
				}

				for _, ref := range snykIssues[0].References {
					vulcanVulnerability.References = append(vulcanVulnerability.References, ref.URL)
				}

				vulns = append(vulns, *vulcanVulnerability)
			}
		}

		state.AddVulnerabilities(vulns...)

		return nil
	}
	c := check.NewCheckFromHandler(checkName, run)

	c.RunAndServe()
}

func createDetails(vulnerabilities []SnykVulnerability) string {
	res := ""
	for _, vulnerability := range vulnerabilities {
		str := "Introduced through: "
		n := len(vulnerability.From)
		for i := 0; i < n; i++ {
			str = str + vulnerability.From[i]
			if i < n-2 {
				str = str + " > "
			}
		}
		res = res + str + "\n"
	}
	return res
}

var regexpRemediationTagBegin = regexp.MustCompile(`(?i)<h2.*id="remediation".*</h2>`)
var regexpRemediationTagEnd = regexp.MustCompile(`(?i)<h2.*>`)
var bluemondayParser = bluemonday.StrictPolicy()

func extractRecommendations(buf []byte) []string {
	markdownParser := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)

	res := []string{}

	bufStr := string(buf)
	bufStr = strings.ReplaceAll(bufStr, "\\\\r\\\\n", "\n")
	bufStr = strings.ReplaceAll(bufStr, "\\\\n", "\n")
	bufStr = strings.ReplaceAll(bufStr, "--|", "---|")

	html := markdown.ToHTML([]byte(bufStr), markdownParser, nil)

	locationTagRemediations := regexpRemediationTagBegin.FindIndex(html)
	if len(locationTagRemediations) > 1 {
		remediationSection := html[locationTagRemediations[1]:]

		locationTagRemediationsEnd := regexpRemediationTagEnd.FindIndex(remediationSection)
		if len(locationTagRemediationsEnd) > 1 {
			remediationSection = remediationSection[:locationTagRemediationsEnd[0]]
		}

		remediationStrLines := strings.Split(string(remediationSection), "\n")
		for _, line := range remediationStrLines {
			if len(line) > 0 {
				aux := bluemondayParser.Sanitize(line)
				aux = strings.Trim(aux, "\n")
				if len(aux) > 0 {
					res = append(res, aux)
				}
			}
		}
	}
	return res
}

var regexpOverviewTagBegin = regexp.MustCompile(`(?i)<h2.*id="overview".*</h2>`)

var regexpDetailsTagBegin = regexp.MustCompile(`(?i)<h2.*id="details".*</h2>`)

var regexpNextH2TagBegin = regexp.MustCompile(`(?i)<h2`)

func extractOverview(buf []byte) string {
	markdownParser := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)

	bufStr := string(buf)
	bufStr = strings.ReplaceAll(bufStr, "\\\\r\\\\n", "\n")
	bufStr = strings.ReplaceAll(bufStr, "\\\\n", "\n")
	bufStr = strings.ReplaceAll(bufStr, "--|", "---|")

	htmlUnsafe := markdown.ToHTML([]byte(bufStr), markdownParser, nil)
	html := bluemondayParser.AllowAttrs("id").OnElements("h2").SanitizeBytes(htmlUnsafe)

	locationTagOverview := regexpOverviewTagBegin.FindIndex(html)
	if len(locationTagOverview) > 1 {
		html = html[locationTagOverview[1]:]
		locationTagNextH2 := regexpNextH2TagBegin.FindIndex(html)
		if len(locationTagNextH2) > 1 {
			html = html[:locationTagNextH2[0]]
		}
		return strings.Trim(string(html), "\n")
	}

	return ""
}
