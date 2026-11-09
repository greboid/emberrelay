# emberrelay

Copies GitHub issues and pull requests to Forgejo on a schedule. The only change
sent back to GitHub is closing an issue and linking the commit that closed it.

Each poll covers every non-archived repository owned by the Forgejo token's
user that has a push mirror to github.com.

- Items open on GitHub are copied. Items already closed when first seen are not.
- Pull requests are pushed to Forgejo with the AGit flow (`refs/for/<base>/gh-<n>`),
  so they are mergeable and create no branch. New GitHub commits update them.
- New comments, reviews and inline review comments are copied.
- Closing or reopening on GitHub is applied to Forgejo.
- Closing a copied issue on Forgejo closes it on GitHub. If a commit closed it,
  a comment linking that commit is added on GitHub. Reopening on Forgejo is not
  copied, and pull requests are not closed on GitHub.
- Title and body edits on GitHub are not copied.

Progress is stored in an HTML comment on the first line of each copied item and
comment, so no volume is needed. Removing that comment causes a duplicate copy.

## Configuration

| Flag              | Environment     | Default | Description                     |
|-------------------|-----------------|---------|---------------------------------|
| `--github-token`  | `GITHUB_TOKEN`  |         | GitHub API token                |
| `--forgejo-token` | `FORGEJO_TOKEN` |         | Forgejo API token               |
| `--forgejo-url`   | `FORGEJO_URL`   |         | Base URL of the Forgejo instance |
| `--poll-interval` | `POLL_INTERVAL` | `1h`    | Time between polls              |
| `--log.level`     | `LOG_LEVEL`     | `info`  | `debug`, `info`, `warn`, `error` |
| `--log.format`    | `LOG_FORMAT`    | `text`  | `text` or `json`                |

The GitHub token needs read access to contents and pull requests, and write
access to issues. The Forgejo token needs `read:user`, `write:repository` and `write:issue`.
