package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
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

var (
	token              = ""
	remindersChannelId = ""
	dbHandle           *sql.DB
)

func bootstrapDb() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "reminders.db")
	if err != nil {
		return db, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS reminders (
			id INTEGER NOT NULL PRIMARY KEY,
			who TEXT NOT NULL,
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
	case "minute", "minutes":
		targetTime = targetTime.Add(time.Minute * time.Duration(n))
	case "hour", "hours":
		targetTime = targetTime.Add(time.Hour * time.Duration(n))
	case "day", "days":
		targetTime = targetTime.AddDate(0, 0, n)
	case "week", "weeks":
		targetTime = targetTime.AddDate(0, 0, 7*n)
	case "month", "months":
		targetTime = targetTime.AddDate(0, n, 0)
	default:
		log.Println("Something went really wrong, we shouldn't be here.")
	}

	return n, units, toRemind, targetTime
}

func messageCreate(session *discordgo.Session, message *discordgo.MessageCreate) {
	// Ignore the bot's own messages.
	if message.Author.ID == session.State.User.ID {
		return
	}

	// Ignore other bots' shenanigans.
	if message.Author.Bot {
		return
	}

	r, err := regexp.Compile(`^!remindme "in (\d{1,2}) (minutes?|hours?|days?|weeks?|months?)" "(.+)"`)
	if err != nil {
		log.Println("Error compiling the regular expression:", err)
	}

	messageMatches := r.MatchString(message.Content)

	if strings.HasPrefix(message.Content, "!remindme") && !messageMatches {
		session.ChannelMessageSendReply(
			message.ChannelID,
			"Invalid !remindme syntax. Has to match the regex "+
				"`^!remindme \"in (\\d{1,2}) (minutes?|hours?|days?|weeks?|months?)\" \"(.+)\"`, e.g. "+
				"`!remindme \"in 2 days\" \"to buy a gift for Chris\"`.",
			message.Reference(),
		)

		return
	}

	if !messageMatches {
		return
	}

	n, units, toRemind, targetTime := parseRemindme(r.FindStringSubmatch(message.Content))
	if n == 0 {
		session.ChannelMessageSendReply(
			message.ChannelID,
			strings.Replace(fmt.Sprintf("Immediately reminding you to %s, you silly goose.", toRemind), " my ", " your ", -1),
			message.Reference(),
		)

		return
	}

	_, err = dbHandle.Exec("INSERT INTO reminders VALUES(NULL,?,?,?)", message.Author.ID, targetTime, strings.Replace(toRemind, " my ", " your ", -1))
	if err != nil {
		log.Println("Error inserting into the database:", err)
		session.ChannelMessageSendReply(
			message.ChannelID,
			"Something went wrong while inserting to the DB. Check the stderr output.",
			message.Reference(),
		)

		return
	}

	session.ChannelMessageSendReply(
		message.ChannelID,
		fmt.Sprintf("Successfully added to the database. I'll remind you in %d %s.", n, units),
		message.Reference(),
	)
}

func handleReminders(botSession *discordgo.Session, ticker *time.Ticker) {
	for currentTime := range ticker.C {
		if _, err := os.Stat("./reminders.db"); errors.Is(err, os.ErrNotExist) {
			log.Println("Database not bootstrapped yet, nothing to check.")
			continue
		}

		rows, err := dbHandle.Query("SELECT * FROM reminders")
		if err != nil {
			log.Println("Error querying the rows when handling the reminders:", err)
		}

		rowsToDelete := []string{}
		for rows.Next() {
			var (
				id       string
				who      string
				time     time.Time
				toRemind string
			)

			if err := rows.Scan(&id, &who, &time, &toRemind); err != nil {
				log.Println("Error scanning the row:", err)
			}

			if currentTime.UTC().After(time) {
				botSession.ChannelMessageSend(remindersChannelId, fmt.Sprintf("<@%s>, reminding you to %s.", who, toRemind))
				rowsToDelete = append(rowsToDelete, id)
			}
		}
		rows.Close()

		placeholders := make([]string, len(rowsToDelete))
		for i := range placeholders {
			placeholders[i] = "?"
		}

		_, err = dbHandle.Exec(
			fmt.Sprintf("DELETE FROM reminders WHERE id IN (%s)", strings.Join(rowsToDelete, ",")),
		)
		if err != nil {
			log.Println("Error deleting the rows:", err)
		}
	}
}

func init() {
	token = os.Getenv("GOPNIK_TOKEN")
	if len(token) == 0 {
		log.Fatalln("Bot token not found. Make sure to set the GOPNIK_TOKEN environment variable.")
	}

	remindersChannelId = os.Getenv("REMINDERS_CHANNEL")
	if len(remindersChannelId) == 0 {
		log.Fatalln("Reminders channel ID not found. Make sure to set the REMINDERS_CHANNEL environment variable.")
	}

	var err error
	dbHandle, err = bootstrapDb()
	if err != nil {
		log.Fatalln("Error bootstrapping the database:", err)
	}
}

func main() {
	defer dbHandle.Close()

	botSession, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalln("Error creating the bot session:", err)
	}

	botSession.AddHandler(messageCreate)

	botSession.Identify.Intents = discordgo.IntentsGuildMessages

	err = botSession.Open()
	if err != nil {
		log.Fatalln("Error opening the WebSocket connection:", err)
	}
	defer botSession.Close()

	ticker := time.NewTicker(time.Minute)
	go handleReminders(botSession, ticker)

	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	fmt.Println("Shutting down...")
	ticker.Stop()
}
