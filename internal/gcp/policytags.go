// Package gcp holds the Google Cloud lookups behind the policy tag resolver.
//
// It is separate from internal/govern so the decision logic there stays
// testable without a project, and so nothing that merely wants to reason about
// access has to link the Google API clients.
package gcp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/datacatalog/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// fineGrainedReader is the permission BigQuery requires to read a column
// classified under a policy tag. It is carried by
// roles/datacatalog.fineGrainedReader.
const fineGrainedReader = "datacatalog.categories.fineGrainedGet"

// SchemaTags reads policy tags from BigQuery table schemas.
//
// Reading a schema needs only metadata access. This never reads table data,
// so the credentials it wants are roles/bigquery.metadataViewer and nothing
// with dataViewer, dataEditor or admin in the name.
type SchemaTags struct {
	client         *bigquery.Client
	defaultProject string
}

// NewSchemaTags connects with Application Default Credentials.
func NewSchemaTags(ctx context.Context, project string) (*SchemaTags, error) {
	if project == "" {
		return nil, fmt.Errorf("a project is required to read table schemas")
	}
	client, err := bigquery.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("connecting to BigQuery project %s: %w", project, err)
	}
	return &SchemaTags{client: client, defaultProject: project}, nil
}

// Close releases the client.
func (s *SchemaTags) Close() error { return s.client.Close() }

// TagsFor returns the policy tags on each column of one table.
//
// A column with no tag is absent from the map rather than present with an
// empty slice, which is what the resolver reads as unrestricted.
func (s *SchemaTags) TagsFor(ctx context.Context, source string) (map[string][]string, error) {
	project, dataset, table, err := splitSource(source, s.defaultProject)
	if err != nil {
		return nil, err
	}

	md, err := s.client.DatasetInProject(project, dataset).Table(table).Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading schema for %s.%s.%s: %w", project, dataset, table, err)
	}

	out := map[string][]string{}
	collectTags(md.Schema, "", out)
	return out, nil
}

// collectTags walks a schema, including nested RECORD fields.
//
// A nested field is addressed by its dotted path, which is how BigQuery names
// it in INFORMATION_SCHEMA and how a model would have to reference it.
func collectTags(schema bigquery.Schema, prefix string, out map[string][]string) {
	for _, field := range schema {
		name := field.Name
		if prefix != "" {
			name = prefix + "." + field.Name
		}
		if field.PolicyTags != nil && len(field.PolicyTags.Names) > 0 {
			out[name] = append([]string(nil), field.PolicyTags.Names...)
		}
		if len(field.Schema) > 0 {
			collectTags(field.Schema, name, out)
		}
	}
}

// splitSource parses the `source` a model wrote against a table.
//
// Accepts `dataset.table` and `project.dataset.table`. The two part form takes
// the engine's configured project, which is what a model written for one
// project means by it.
func splitSource(source, defaultProject string) (project, dataset, table string, err error) {
	parts := strings.Split(strings.Trim(source, "`"), ".")
	switch len(parts) {
	case 2:
		return defaultProject, parts[0], parts[1], nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	default:
		return "", "", "", fmt.Errorf(
			"source %q is not a BigQuery table; expected dataset.table or project.dataset.table", source)
	}
}

// CatalogAccess answers whether an identity may read under a policy tag, by
// asking Data Catalog as that identity.
//
// It impersonates rather than evaluating the tag's IAM policy locally.
// Evaluating locally would miss bindings inherited from the project, folder and
// organization, conditional bindings, and group expansion, so it would report
// access the warehouse refuses and refuse access it allows. The only correct
// answer to "may this principal read" is the one the platform gives when asked
// as that principal.
type CatalogAccess struct {
	// clients caches one impersonated Data Catalog client per identity.
	// Minting an impersonated token is a network round trip.
	mu      sync.Mutex
	clients map[string]*datacatalog.Service
}

// NewCatalogAccess builds the checker.
//
// The process's own credentials need roles/iam.serviceAccountTokenCreator on
// the identities it will act as.
func NewCatalogAccess() *CatalogAccess {
	return &CatalogAccess{clients: map[string]*datacatalog.Service{}}
}

// CanReadTag reports whether id holds fine grained reader on the policy tag.
func (c *CatalogAccess) CanReadTag(ctx context.Context, id govern.Identity, policyTag string) (bool, error) {
	if id.Subject == "" {
		// An unauthenticated caller reads nothing tagged. Returning true here
		// would make every anonymous request a full access request.
		return false, nil
	}
	if !strings.Contains(policyTag, "/policyTags/") {
		return false, fmt.Errorf("%q is not a policy tag resource name", policyTag)
	}

	service, err := c.serviceFor(ctx, id)
	if err != nil {
		return false, err
	}

	// Asked as the impersonated identity, so the answer is that identity's
	// effective access rather than this process's.
	resp, err := service.Projects.Locations.Taxonomies.PolicyTags.TestIamPermissions(
		policyTag,
		&datacatalog.TestIamPermissionsRequest{Permissions: []string{fineGrainedReader}},
	).Context(ctx).Do()
	if err != nil {
		return false, fmt.Errorf("asking Data Catalog whether %s may read a tagged column: %w", id.Subject, err)
	}

	// An empty list means the permission is not held. It is never an error.
	return slices.Contains(resp.Permissions, fineGrainedReader), nil
}

// serviceFor returns a Data Catalog client acting as id.
func (c *CatalogAccess) serviceFor(ctx context.Context, id govern.Identity) (*datacatalog.Service, error) {
	c.mu.Lock()
	cached, ok := c.clients[id.Subject]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	source, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: id.Subject,
		Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err == nil {
		// Minting one token here rather than waiting for the first request to
		// do it. CredentialsTokenSource builds happily for an identity that
		// cannot be impersonated at all and only fails when something asks it
		// for a token, so without this the failure arrives from deep inside a
		// Data Catalog call and is indistinguishable from Data Catalog being
		// down. That difference decides whether an agent retries forever.
		//
		// It costs nothing: the token source caches, so this is the same mint
		// the first real call would have done.
		_, err = source.Token()
	}
	if err != nil {
		// Failing rather than falling back to this process's credentials: the
		// fallback would answer with the engine's access, which is exactly the
		// shared admin account this project refuses to be.
		//
		// A refusal rather than a plain error, because the gate classifies a
		// bare error as "the policy source is down, try later" and an agent
		// obeys that literally. This failure is a deployment that cannot act
		// as this caller, which no amount of waiting fixes: a human identity
		// can never be impersonated, so the same caller would retry forever.
		return nil, &plan.Refusal{
			Code: govern.CodePolicyIdentity,
			Reason: fmt.Sprintf("this engine cannot act as %s, so column access could not be"+
				" resolved and the query was not run", id.Subject),
			Hint: "grant this deployment roles/iam.serviceAccountTokenCreator on that" +
				" identity. Only a service account can be impersonated, so a human caller" +
				" needs a policy file rather than policy tags.",
		}
	}

	service, err := datacatalog.NewService(ctx, option.WithTokenSource(source))
	if err != nil {
		return nil, fmt.Errorf("building a Data Catalog client for %s: %w", id.Subject, err)
	}

	c.mu.Lock()
	c.clients[id.Subject] = service
	c.mu.Unlock()
	return service, nil
}
