## grafanapi config

View or manipulate configuration settings

### Synopsis

View or manipulate configuration settings.

The configuration file to load is chosen as follows:

1. If the --config flag is set, then that file will be loaded. No other location will be considered.
2. If the $GRAFANAPI_CONFIG environment variable is set, then that file will be loaded. No other location will be considered.
3. If the $XDG_CONFIG_HOME environment variable is set, then it will be used: $XDG_CONFIG_HOME/grafanapi/config.yaml
   Example: /home/user/.config/grafanapi/config.yaml
4. If the $HOME environment variable is set, then it will be used: $HOME/.config/grafanapi/config.yaml
   Example: /home/user/.config/grafanapi/config.yaml
5. If the $XDG_CONFIG_DIRS environment variable is set, then it will be used: $XDG_CONFIG_DIRS/grafanapi/config.yaml
   Example: /etc/xdg/grafanapi/config.yaml


### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
  -h, --help             help for config
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 
* [grafanapi config check](grafanapi_config_check.md)	 - Check the current configuration for issues
* [grafanapi config current-context](grafanapi_config_current-context.md)	 - Display the current context name
* [grafanapi config list-contexts](grafanapi_config_list-contexts.md)	 - List the contexts defined in the configuration
* [grafanapi config set](grafanapi_config_set.md)	 - Set an single value in a configuration file
* [grafanapi config unset](grafanapi_config_unset.md)	 - Unset an single value in a configuration file
* [grafanapi config use-context](grafanapi_config_use-context.md)	 - Set the current context
* [grafanapi config view](grafanapi_config_view.md)	 - Display the current configuration

