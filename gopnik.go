package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	_ "github.com/mattn/go-sqlite3"
)

func bootstrapDb() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "reminders.db")
	if err != nil {
		return db, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS reminders (
			id INTEGER NOT NULL PRIMARY KEY,
			time DATETIME NOT NULL,
			toRemind TEXT NOT NULL
		);`)
	if err != nil {
		return db, err
	}

	return db, nil
}

func parseRemindme(matches []string) (int, string, string, time.Time) {
	n, _ := strconv.Atoi(matches[1])
	units, toRemind := matches[2], matches[3]

	targetTime := time.Now().UTC()
	switch units {
	case "minute":
	case "minutes":
		targetTime = targetTime.Add(time.Minute * time.Duration(n))
	case "hour":
	case "hours":
		targetTime = targetTime.Add(time.Hour * time.Duration(n))
	case "day":
	case "days":
		targetTime = targetTime.Add(time.Hour * 24 * time.Duration(n))
	case "week":
	case "weeks":
		targetTime = targetTime.Add(time.Hour * 24 * 7 * time.Duration(n))
	case "month":
	case "months":
		targetTime = targetTime.Add(time.Hour * 24 * 30 * time.Duration(n))
	default:
		fmt.Fprintln(os.Stderr, "Something went really wrong, we shouldn't be here.")
	}

	return n, units, toRemind, targetTime
}

func messageCreate(session *discordgo.Session, message *discordgo.MessageCreate) {
	// Ignore bot's own messages.
	if message.Author.ID == session.State.User.ID {
		return
	}

	r, err := regexp.Compile(`^!remindme "in (\d+) (minutes?|hours?|days?|weeks?|months?)" "(.+)"`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error compiling the regular expression: ", err)
	}

	messageMatches := r.MatchString(message.Content)

	if strings.HasPrefix(message.Content, "!remindme") && !messageMatches {
		session.ChannelMessageSendReply(
			message.ChannelID,
			"Invalid !remindme syntax. Has to match the regex "+
				"`^!remindme \"in (\\d+) (minutes?|hours?|days?|weeks?|months?)\" \"(.+)\"`, e.g. "+
				"`!remindme \"in 2 days\" \"to buy a gift for Chris\"`.",
			message.Reference(),
		)

		return
	}

	if !messageMatches {
		return
	}

	n, units, toRemind, targetTime := parseRemindme(r.FindStringSubmatch(message.Content))

	db, err := bootstrapDb()
	if err != nil {
		db.Close()

		fmt.Fprintln(os.Stderr, "Error bootstrapping the database: ", err)
		session.ChannelMessageSendReply(
			message.ChannelID,
			"Something went wrong with bootstrapping the DB. Check the stderr output.",
			message.Reference(),
		)

		return
	}
	defer db.Close()

	_, err = db.Exec("INSERT INTO reminders VALUES(NULL,?,?)", targetTime, toRemind)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error inserting into the database: ", err)
		session.ChannelMessageSendReply(
			message.ChannelID,
			"Something went wrong with inserting to the DB. Check the stderr output.",
			message.Reference(),
		)

		return
	}

	session.ChannelMessageSendReply(
		message.ChannelID,
		fmt.Sprintf("Successfully added to the database. I'll remind you in %d %s %s.", n, units, toRemind),
		message.Reference(),
	)
}

func main() {
	token := os.Getenv("GOPNIK_TOKEN")
	if len(token) == 0 {
		fmt.Fprintln(os.Stderr, "Bot token not found. Make sure to set the GOPNIK_TOKEN environment variable.")
		os.Exit(42)
	}

	botSession, err := discordgo.New("Bot " + token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating the bot session: ", err)
		os.Exit(42)
	}

	botSession.AddHandler(messageCreate)

	botSession.Identify.Intents = discordgo.IntentsGuildMessages

	err = botSession.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error opening the WebSocket connection: ", err)
		os.Exit(42)
	}
	defer botSession.Close()

	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	fmt.Println("Shutting down...")
}
