# Environment variables reference

## `GRAFANA_ORG_ID`

OrgID specifies the organization targeted by this config.
Note: required when targeting an on-prem Grafana instance.
See StackID for Grafana Cloud instances.

## `GRAFANA_SERVER`

Server is the address of the Grafana server (https://hostname:port/path).
Required. It must use https://; plain http:// is only accepted for a Grafana running on
this machine (localhost, any 127.x.x.x address, or ::1). Internationalised host names must be
given in punycode (xn--...).

## `GRAFANA_STACK_ID`

StackID specifies the Grafana Cloud stack targeted by this config.
Note: required when targeting a Grafana Cloud instance.
See OrgID for on-prem Grafana instances.
