# agentd-proxy

A proxy for agentd that handles authentication and authorization to desktops in the hub.


## Setup

Requires the following environment variables:

* `HUB_AUTH_ADDR` - The address of the hub auth server
* `DB_HOST` - The address of the hub database
* `DB_USER` - The username for the hub database
* `DB_PASSWORD` - The password for the hub database
* `DB_NAME` - The name of the hub database