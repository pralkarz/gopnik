# gopnik

`gopnik` (funny word + the app is written in Go) is a tiny Discord bot that reminds users about various things they ask it to.

![example usage](https://github.com/user-attachments/assets/aaba9865-e91e-4460-be74-6f66356d8c9f)

# usage
![how to use](https://github.com/user-attachments/assets/d23f19fd-1bac-480d-8ad6-f2460060613d)

# setup

1. Set up an application in [Discord Developer Portal](https://discord.com/developers/applications) and add the bot to your server. Below are the required scopes and permissions. Additionally, you need to enable the Message Content Intent.

   ![image](https://github.com/user-attachments/assets/d6c3c795-34b9-49a0-8664-efc7f9d835da)

2. Clone this repository. The reminders are managed with https://github.com/mattn/go-sqlite3, therefore before installing the dependencies, you need to set the `CGO_ENABLED=1` env variable and have `gcc` available in your PATH.
3. Run `go mod tidy` to download and install the dependencies.
4. Set the `GOPNIK_TOKEN` and `REMINDERS_CHANNEL` environment variables to your bot's token and the ID of the channel where it should send the reminders, respectively.
5. Run `go run .` or `go build . && ./gopnik`. Errors are written to stderr: I personally redirect them to a file with `./gopnik 2>> logs`.


