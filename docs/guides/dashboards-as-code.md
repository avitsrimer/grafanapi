---
title: Dashboards as code
---

With this workflow, you can define and manage dashboards as code, saving them to a version control system like Git. This is useful for teams that want to maintain a history of changes, collaborate on dashboard design, and ensure consistency across environments.

!!! note
    In order to use `grafanapi resources serve` functionality, you will have to enable a feature toggle in your `config.ini`:
      ```ini
      [feature_toggles]
      kubernetesDashboards = true
      ```
    Check [the documentation](https://grafana.com/docs/grafana/latest/setup-grafana/configure-grafana/#feature_toggles) to learn more about enabling feature toggles.

1. Use a dashboard generation script (for example, with the [Foundation SDK](https://github.com/grafana/grafana-foundation-sdk)). You can find an example implementation in the Grafana as code [hands-on lab repository](https://github.com/grafana/dashboards-as-code-workshop/tree/main/part-one-golang-starter).
1. Serve and preview the output of the dashboard generator locally:
   ```shell
   grafanapi config use-context YOUR_CONTEXT  # for example "dev"
   grafanapi resources serve --script 'go run scripts/generate-dashboard.go' --watch './scripts'
   ```
   Open the URL printed at startup (`http://127.0.0.1:8080/?token=...`). The token is exchanged for a cookie on first visit and changes on every run, so a bookmark of `http://127.0.0.1:8080/` returns `401` after a restart. Non-browser clients can pass the cookie instead, for example `curl -b grafanapi_serve_8080=<token> http://127.0.0.1:8080/`.

   !!! warning
       The server listens on `127.0.0.1` by default and acts with your Grafana session. Passing a non-loopback `--address` (for example `0.0.0.0`) makes it reachable from other machines and prints a warning. A wildcard bind accepts only the interface IPs present at startup, so a changed IP (VPN, DHCP) yields `421` until you restart. The token cookie is a session cookie, and browsers send cookies by host, not port: any other server on the same host that you visit in that browser receives it, so do not rely on the token alone on a shared machine.
1. When the output looks correct, generate dashboard manifest files:
   ```shell
   go run scripts/generate-dashboard.go --generate-resource-manifests --output './resources'
   ```
1. Push the generated resources to your Grafana instance:
   ```shell
   grafanapi config use-context YOUR_CONTEXT  # for example "dev"
   grafanapi resources push -d ./resources/
   ```
