package main

import (
	"fmt"
	"github.com/hashicorp/go-version"
	"github.com/jessevdk/go-flags"
	"github.com/karrick/tparse"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xanzy/go-gitlab"
	"os"
	"time"
)

var options struct {
	API     string `long:"url" required:"true" description:"Gitlab API endpoint"`
	Token   string `long:"token" required:"true" description:"Gitlab API access token"`
	Group   string `long:"group" required:"false" description:"Gitlab group for filter"`
	Project string `long:"project" required:"false" description:"Gitlab project for tagging"`
	Search  string `long:"search" required:"false" description:"Gitlab projects search key"`
	Forced  bool   `long:"force" required:"false" description:"Forced to re-tag (last tag or v1.0.0)"`
	Expired string `long:"expired" required:"false" default:"now-1d" description:"Expired time to re-tag (diff with latest tag)"`
	DryRun  bool   `long:"dry-run" required:"false"`
	Debug   bool   `long:"debug" required:"false"`
}

const PageOnceMax = 999
const ProtectedTagExpr = "v*"
const DefaultBranchName = "master"

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	if _, err := flags.Parse(&options); err != nil {
		return
	} else if !options.Debug {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	gapi := gitlab.NewClient(nil, options.Token)
	if err := gapi.SetBaseURL(options.API); err != nil {
		panic(err)
	}

	var expired time.Time
	if options.Expired != "" {
		var err error
		if expired, err = tparse.ParseNow(time.RFC3339, options.Expired); err != nil {
			panic(err)
		}
	}

	projects := make([]*gitlab.Project, 0)

	if options.Project != "" {
		if project, _, err := gapi.Projects.GetProject(options.Project, &gitlab.GetProjectOptions{}); err != nil {
			panic(err)
		} else {
			projects = append(projects, project)
		}
	} else if options.Group != "" {
		if group, _, err := gapi.Groups.GetGroup(options.Group); err != nil {
			panic(err)
		} else {
			for _, group := range exploreSubGroups(gapi, group) {
				projects = append(projects, exploreGroupProjects(gapi, group, options.Search)...)
			}
		}
	} else if options.Search != "" {
		if list, _, err := gapi.Projects.ListProjects(&gitlab.ListProjectsOptions{
			ListOptions: gitlab.ListOptions{PerPage: PageOnceMax},
			Simple:      gitlab.Bool(true),
			Search:      gitlab.String(options.Search),
		}); err == nil {
			projects = append(projects, list...)
		}
	}

	for _, project := range projects {
		projectTagging(gapi, project, options.Forced, expired, !options.DryRun)
	}
}

func exploreSubGroups(api *gitlab.Client, group *gitlab.Group) []*gitlab.Group {
	log.Printf("Exploring sub-groups in %s", group.FullPath)

	groups := []*gitlab.Group{group}

	list, _, err := api.Groups.ListSubgroups(
		group.ID,
		&gitlab.ListSubgroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: PageOnceMax},
		},
	)

	if err != nil {
		log.Warn().Err(err)
	}

	for _, nest := range list {
		groups = append(groups, exploreSubGroups(api, nest)...)
	}

	return groups
}

func exploreGroupProjects(api *gitlab.Client, group *gitlab.Group, search string) []*gitlab.Project {
	log.Printf("Exploring projects in %s", group.FullPath)

	options := &gitlab.ListGroupProjectsOptions{
		ListOptions: gitlab.ListOptions{PerPage: PageOnceMax},
		Simple:      gitlab.Bool(true),
	}

	if search != "" {
		options.Search = gitlab.String(search)
	}

	if list, _, err := api.Groups.ListGroupProjects(group.ID, options); err == nil {
		return list
	}

	return nil
}

func projectTagging(api *gitlab.Client, project *gitlab.Project, forced bool, expired time.Time, do bool) {
	log.Printf("[%s] Start to process tags", project.PathWithNamespace)

	tags, _, err := api.Tags.ListTags(project.ID, &gitlab.ListTagsOptions{ListOptions: gitlab.ListOptions{PerPage: 1}})
	if err != nil {
		panic(err)
	}

	last := "*NEVER*"
	next := ""
	if len(tags) > 0 {
		latest := tags[0]

		commits, _, err := api.Commits.ListCommits(project.ID, &gitlab.ListCommitsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 1},
			RefName:     gitlab.String(DefaultBranchName),
		})
		if err != nil {
			panic(err)
		}

		commit := commits[0]
		if commit.ID == latest.Commit.ID {
			log.Info().Msgf("[%s] No new commits submitted -> skip / latest is %s", project.PathWithNamespace, commit.ShortID)
			return
		}

		last = latest.Name

		if forced {
			next = latest.Name
		} else if !expired.IsZero() && latest.Commit.CommittedDate.Sub(expired) > 0 {
			next = latest.Name
		} else {
			if ver, err := version.NewVersion(latest.Name); err != nil {
				panic(err)
			} else {
				segs := ver.Segments()
				next = fmt.Sprintf("v%d.%d.%d", segs[0], segs[1], segs[2]+1)
			}
		}
	} else {
		next = "v1.0.0"
	}

	created := "SKIP(dry-run)"
	if do {
		tagsUnprotected(api, project)
		defer tagsProtected(api, project)

		if last == next {
			if _, err := api.Tags.DeleteTag(project.ID, next); err != nil {
				panic(err)
			} else {
				log.Warn().Msgf("[%s] Previous TAG:%s has been deleted", project.PathWithNamespace, last)
			}
		}

		if tag, _, err := api.Tags.CreateTag(project.ID, &gitlab.CreateTagOptions{
			TagName: gitlab.String(next),
			Ref:     gitlab.String(DefaultBranchName),
		}); err != nil {
			panic(err)
		} else {
			created = fmt.Sprintf("DONE(%s:%s)", tag.Commit.ShortID, tag.Commit.Message)
		}
	}

	log.Info().Msgf("[%s] Tags will creating %s -> %s -> %s", project.PathWithNamespace, last, next, created)
}

func tagsProtected(api *gitlab.Client, project *gitlab.Project) {
	if protected, _, err := api.ProtectedTags.ProtectRepositoryTags(project.ID, &gitlab.ProtectRepositoryTagsOptions{
		Name: gitlab.String(ProtectedTagExpr),
	}); err != nil {
		panic(err)
	} else {
		log.Printf("[%s] Repository tags protected in %s", project.PathWithNamespace, protected.Name)
	}
}

func tagsUnprotected(api *gitlab.Client, project *gitlab.Project) {
	if protected, resp, err := api.ProtectedTags.GetProtectedTag(project.ID, ProtectedTagExpr); err != nil {
		if resp.StatusCode != 404 {
			panic(err)
		}
	} else {
		if _, err := api.ProtectedTags.UnprotectRepositoryTags(project.ID, ProtectedTagExpr); err != nil {
			panic(err)
		} else {
			log.Printf("[%s] Found and unprotected tag expr = %s", project.PathWithNamespace, protected.Name)
		}
	}
}
