# ⚠️ Warning:

**Strafe `Equinox` (Backend) and all other projects related to this such as `Frontend, Nebula (FileSystem), etc.` are still under heavy development. We reccommend that you not try to use it for personal use until development has progressed more and bugs are less likely to happen.**

# Equinox
Equinox is the code name for the current version of our backend in NodeJS.

## Setup Locally

```bash
# Clone the repo
git clone https://github.com/StrafeChat/equinox

# Install all dependencies
npm i

# run the backend
npm run dev
```

## Environment Variables
Find .env.example

## Routes
Express webserver
Currently the backend supports the following routes:

| Request Method 	| Url                                  	| Description *(Usage)*                                 	|
|----------------	|--------------------------------------	|-------------------------------------------------------	|
| **POST**       	| `/v1/auth/register`                  	| Creating an account                                   	|
| **GET**        	| `/v1/users/@me`                      	| Fetch you your personal user data                     	|
| **GET**        	| `/v1/users/@me/relationships`        	| Fetch your *friends* and *friend requests*            	|
| **PATCH**      	| `/v1/users/@me/relationships/:query` 	| Update friend requests, accept / reject them          	|
| **POST**       	| `/v1/users/@me/relationships/:query` 	| Create / Send a friend request                        	|
| **DELETE**     	| `/v1/users/@me/relationships/:query` 	| Delete / Revoke a friend request / Remove a friend    	|
| **POST**       	| `/v1/users/@me/rooms`                	| Create a PM *(Private Message)* with another **user** 	|

## Websocket
This project holds our HTTP API as well as our events websocket.

You can connect to the websocket on this webserver with your domain, localhost:443 for example. With the /events pathname.
- Ex. ws://localhost:443/events
- Production: wss://api.strafe.chat/events
