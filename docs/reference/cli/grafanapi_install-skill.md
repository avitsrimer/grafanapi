## grafanapi install-skill

Install the bundled Claude Code skill

### Synopsis

Install the bundled grafanapi Claude Code skill into a .claude folder.

The skill teaches Claude Code (or any agent reading the Claude Code skill format) how to use
grafanapi: authentication, discovering datasources, running ad-hoc queries, and managing
dashboards/folders as code. It is embedded in the grafanapi binary at build time, so this command
works offline and from any directory, independent of the source checkout.

Any existing installation at the destination is replaced.

```
grafanapi install-skill [flags]
```

### Examples

```

	grafanapi install-skill
	grafanapi install-skill --to ~/projects/my-repo/.claude
```

### Options

```
  -h, --help        help for install-skill
      --to string   Path to a .claude folder (default ~/.claude)
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 

