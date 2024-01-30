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

- **POST** `/v1/auth/login` - Used for logging.
- **POST** `/v1/auth/register` - User for creating an account.
- **GET** `/v1/users/@me` - Used to send you your personal user data.
- **GET** `/v1/users/@me/relationships` - Used to fetch your friends and friend requests.
- **PATCH** `/v1/users/@me/relationships/:query` - Used to update friend requests, accept /reject.
- **POST** `/v1/users/@me/relationships/:query` - Used to create a friend request.
- **DELETE** `/v1/users/@me/relationships/:query` - Used to delete a friend request or remove a friend.
- **POST** `/v1/users/@me/rooms` - Used to create a PM (Private Message) with another user.

## Websocket
This project holds our HTTP API as well as our events websocket.

You can connect to the websocket on this webserver with your domain, localhost:443 for exmaple. With the /events pathname.
- Ex. ws://localhost:443/events
- Production: wss://stargate.strafe.chat
