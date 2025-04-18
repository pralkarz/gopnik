package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
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

type rowToUpdate struct {
	id      string
	newTime time.Time
	who     string
}

type eventState struct {
	session *discordgo.Session
	message *discordgo.MessageCreate
}

func (es *eventState) reply(msg string) {
	es.session.ChannelMessageSendReply(es.message.ChannelID, msg, es.message.Reference())
}

func isLeapYear(year int) bool {
	if year%400 == 0 {
		return true
	}

	return year%4 == 0 && year%100 != 0
}

func handlePendingReminders(es *eventState) {
	rows, err := dbHandle.Query("SELECT * FROM Reminders WHERE who=? ORDER BY time", es.message.Author.ID)
	if err != nil {
		log.Println("Error querying the pending reminders:", err)
		es.reply("Something went wrong while querying the pending reminders. Check the stderr output.")
		return
	}
	defer rows.Close()

	reminders := make([]string, 0)
	for rows.Next() {
		var (
			id        uint32
			who       string
			time      time.Time
			toRemind  string
			recurring bool
		)

		if err := rows.Scan(&id, &who, &time, &toRemind, &recurring); err != nil {
			log.Println("Error scanning the row:", err)
		}

		if recurring {
			reminders = append(reminders, fmt.Sprintf("*[ID: %d]* %s every day, next time on <t:%d>", id, toRemind, time.Unix()))
		} else {
			reminders = append(reminders, fmt.Sprintf("*[ID: %d]* %s on <t:%d>", id, toRemind, time.Unix()))
		}
	}
	if err = rows.Err(); err != nil {
		log.Println("Error when iterating over the pending reminders:", err)
		es.reply("Something went wrong while iterating over the pending reminders. Check the stderr output.")
		return
	}

	if len(reminders) == 0 {
		es.reply("You have no pending reminders.")
		return
	}

	var pendingReminders strings.Builder
	pendingReminders.WriteString("You have the following pending reminders:\n")
	for idx, reminder := range reminders {
		pendingReminders.WriteString(fmt.Sprintf("%d. Reminder %s.\n", idx+1, reminder))
	}
	pendingReminders.WriteString("\nTo remove a reminder, use `!rmreminder <ID>`, e.g. `!rmreminder 42`.")

	es.reply(pendingReminders.String())
}

func handleTzpreferenceRegexMatch(es *eventState, matches []string) {
	newTzPreference, err := time.LoadLocation(matches[1])
	if err != nil {
		log.Println("Error loading the location:", err)
		es.reply("Something went wrong while loading the location. Make sure it's correct or check the stderr output.")
		return
	}

	var (
		id                   string
		who                  string
		existingTzPreference string
	)

	err = dbHandle.QueryRow("SELECT * FROM TimezonePreferences WHERE who=?", es.message.Author.ID).Scan(&id, &who, &existingTzPreference)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, err = dbHandle.Exec("INSERT INTO TimezonePreferences VALUES(NULL,?,?)", es.message.Author.ID, newTzPreference.String())
			if err != nil {
				log.Println("Error inserting into the database:", err)
				es.reply("Something went wrong while inserting to the DB. Check the stderr output.")
				return
			}
		} else {
			log.Println("Error querying for a previously saved preference:", err)
			es.reply("Something went wrong while querying for a previously saved preference. Check the stderr output.")
			return
		}
	}

	_, err = dbHandle.Exec("UPDATE TimezonePreferences SET timezonePreference=? WHERE id=?", newTzPreference.String(), id)
	if err != nil {
		log.Println("Error updating the database:", err)
		es.reply("Something went wrong while updating the DB. Check the stderr output.")
		return
	}

	es.reply("Successfully set the preference.")
}

func handleRmreminderRegexMatch(es *eventState, matches []string) {
	id, _ := strconv.Atoi(matches[1])
	if id > math.MaxUint32 {
		es.reply(fmt.Sprintf("The ID is too big, has to be between 0 and %d.", math.MaxUint32))
		return
	}

	var who string
	err := dbHandle.QueryRow("SELECT who FROM Reminders WHERE id=?", id).Scan(&who)
	if errors.Is(err, sql.ErrNoRows) {
		es.reply("There isn't a reminder with that ID. Make sure you provided the correct one.")
		return
	}

	if es.message.Author.ID != who {
		es.reply("You cannot remove someone else's reminders!")
		return
	}

	_, err = dbHandle.Exec("DELETE FROM Reminders WHERE id=?", id)
	if err != nil {
		log.Println("Error deleting the row:", err)
		es.reply("Something went wrong while deleting the reminder. Check the stderr output.")
	}

	es.reply("Successfully deleted the reminder.")
}

func isAbsoluteDateValid(day int, month int, year int, hour int, minute int, currentYear int) (string, bool) {
	// Validate day and month.
	if day == 0 || day > 31 {
		return fmt.Sprintf("No month has %d days you silly goose.", day), false
	} else if month == 0 {
		return "There is no 0th month my dear pumpkin.", false
	} else if month > 12 {
		return "There aren't that many months!", false
	} else {
		daysInMonths := [12]int{31, 0, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

		if isLeapYear(year) {
			daysInMonths[1] = 29
		} else {
			daysInMonths[1] = 28
		}

		if day > daysInMonths[month-1] {
			return fmt.Sprintf("There aren't %d days in this month.", day), false
		}
	}

	// Validate year.
	difference := year - currentYear
	if difference < 0 || difference > 1 {
		return fmt.Sprintf("The year has to be either %d or %d.", currentYear, currentYear+1), false
	}

	// Validate hour.
	if hour == 0 || hour > 12 {
		return "The time has to follow the [12-hour clock](https://en.wikipedia.org/wiki/12-hour_clock).", false
	}

	// Validate minute.
	if minute > 59 {
		return "Are you sure you understand the clock?", false
	}

	return "", true
}

// Resolves the location with the following precedence:
// 1. Explicitly specified in the command.
// 2. Read from the TimezonePreferences table.
// 3. Default (Europe/Warsaw).
func resolveLocation(es *eventState, locationMatch string) (*time.Location, error) {
	if len(locationMatch) > 0 {
		return time.LoadLocation(locationMatch)
	} else {
		var (
			id                   string
			who                  string
			existingTzPreference string
		)

		err := dbHandle.QueryRow("SELECT * FROM TimezonePreferences WHERE who=?", es.message.Author.ID).Scan(&id, &who, &existingTzPreference)
		if errors.Is(err, sql.ErrNoRows) {
			return time.LoadLocation("Europe/Warsaw")
		} else {
			return time.LoadLocation(existingTzPreference)
		}
	}
}

func handleAbsoluteRegexMatch(es *eventState, matches []string) {
	toRemind := matches[8]
	if len(toRemind) > 1500 {
		es.reply("The maximum reminder length is 1500 characters, you naughty person.")
		return
	}

	location, err := resolveLocation(es, matches[7])
	if err != nil {
		log.Println("Error resolving the location:", err)
		es.reply("Couldn't resolve your location. Make sure you spelled it correctly or check the stderr output.")
		return
	}

	currentTime := time.Now().In(location)
	currentYear := currentTime.Year()

	day, _ := strconv.Atoi(matches[1])
	month, _ := strconv.Atoi(matches[2])

	var year int
	if len(matches[3]) > 0 {
		year, _ = strconv.Atoi(matches[3])
	} else {
		year = currentYear
	}

	hour, _ := strconv.Atoi(matches[4])

	var minute int
	if len(matches[5]) > 0 {
		minute, _ = strconv.Atoi(matches[5])
	} else {
		minute = 0
	}

	if errMsg, ok := isAbsoluteDateValid(day, month, year, hour, minute, currentYear); !ok {
		es.reply(errMsg)
		return
	}

	period := matches[6]

	// Hour in the 12-hour format is needed later for the information for the user,
	// but the database expects the 24-hour format.
	dbHour := hour
	if period == "AM" && hour == 12 {
		dbHour = 0
	} else if period == "PM" && hour < 12 {
		dbHour += 12
	}

	targetTime, err := time.ParseInLocation(
		time.DateTime,
		fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d", year, month, day, dbHour, minute, 0),
		location,
	)
	if err != nil {
		log.Println("Error parsing the time:", err)
		es.reply("Something went wrong while parsing the time. Check the stderr output.")
		return
	}

	targetTime = targetTime.UTC()
	if targetTime.Before(currentTime.UTC()) {
		es.reply("The date cannot be in the past, who would've guessed?")
		return
	}

	_, err = dbHandle.Exec("INSERT INTO Reminders VALUES(NULL,?,?,?,0)", es.message.Author.ID, targetTime, strings.Replace(toRemind, " my ", " your ", -1))
	if err != nil {
		log.Println("Error inserting into the database:", err)
		es.reply("Something went wrong while inserting to the DB. Check the stderr output.")
		return
	}

	reply := fmt.Sprintf("Successfully added to the database. I'll remind you %s on %02d.%02d.%d at %02d:%02d %s in the %s timezone.",
		toRemind, day, month, year, hour, minute, period, location.String())
	es.reply(strings.Replace(reply, " my ", " your ", -1))
}

func parseRelativeRemindme(matches []string) (int, string, string, time.Time) {
	var n int
	if matches[1] == "a" || matches[1] == "an" {
		n = 1
	} else {
		n, _ = strconv.Atoi(matches[1])
	}

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

func handleRelativeRegexMatch(es *eventState, matches []string) {
	n, units, toRemind, targetTime := parseRelativeRemindme(matches)
	if len(toRemind) > 1500 {
		es.reply("The maximum reminder length is 1500 characters.")
		return
	}

	if n == 0 {
		es.reply(strings.Replace(fmt.Sprintf("Immediately reminding you %s, you silly goose.", toRemind), " my ", " your ", -1))
		return
	}

	parsedToRemind := strings.Replace(toRemind, " my ", " your ", -1)
	_, err := dbHandle.Exec("INSERT INTO Reminders VALUES(NULL,?,?,?,0)", es.message.Author.ID, targetTime, parsedToRemind)
	if err != nil {
		log.Println("Error inserting into the database:", err)
		es.reply("Something went wrong while inserting to the DB. Check the stderr output.")
		return
	}

	es.reply(fmt.Sprintf("Successfully added to the database. I'll remind you in %d %s %s.", n, units, parsedToRemind))
}

func handleRecurringRegexMatch(es *eventState, matches []string) {
	toRemind := matches[5]
	if len(toRemind) > 1500 {
		es.reply("The maximum reminder length is 1500 characters, you naughty person.")
		return
	}

	location, err := resolveLocation(es, matches[4])
	if err != nil {
		log.Println("Error resolving the location:", err)
		es.reply("Couldn't resolve your location. Make sure you spelled it correctly or check the stderr output.")
		return
	}

	currentTime := time.Now().In(location)

	hour, _ := strconv.Atoi(matches[1])

	var minute int
	if len(matches[2]) > 0 {
		minute, _ = strconv.Atoi(matches[2])
	} else {
		minute = 0
	}

	if errMsg, ok := isAbsoluteDateValid(currentTime.Day(), int(currentTime.Month()), currentTime.Year(), hour, minute, currentTime.Year()); !ok {
		es.reply(errMsg)
		return
	}

	period := matches[3]
	// Hour in the 12-hour format is needed later for the information for the user,
	// but the database expects the 24-hour format.
	dbHour := hour
	if period == "AM" && hour == 12 {
		dbHour = 0
	} else if period == "PM" && hour < 12 {
		dbHour += 12
	}

	targetTime, err := time.ParseInLocation(
		time.DateTime,
		fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d", currentTime.Year(), currentTime.Month(), currentTime.Day(), dbHour, minute, 0),
		location,
	)
	if err != nil {
		log.Println("Error parsing the time:", err)
		es.reply("Something went wrong while parsing the time. Check the stderr output.")
		return
	}

	targetTime = targetTime.UTC()
	if targetTime.Before(currentTime.UTC()) {
		targetTime = targetTime.AddDate(0, 0, 1)
	}

	_, err = dbHandle.Exec("INSERT INTO Reminders VALUES(NULL,?,?,?,1)", es.message.Author.ID, targetTime, strings.Replace(toRemind, " my ", " your ", -1))
	if err != nil {
		log.Println("Error inserting into the database:", err)
		es.reply("Something went wrong while inserting to the DB. Check the stderr output.")
		return
	}

	reply := fmt.Sprintf("Successfully added to the database. I'll remind you %s every day at %02d:%02d %s in the %s timezone.",
		toRemind, hour, minute, period, location.String())
	es.reply(strings.Replace(reply, " my ", " your ", -1))
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

	if !strings.HasPrefix(message.Content, "!") {
		return
	}

	eventState := eventState{session, message}

	if message.Content == "!reminders" {
		handlePendingReminders(&eventState)
		return
	}

	const tzpreferenceRegex = `^!tzpreference ([a-zA-Z]+\/[a-zA-Z_]+)$`
	tzpreferenceRegexCompiled := regexp.MustCompile(tzpreferenceRegex)

	doesTzpreferenceRegexMatch := tzpreferenceRegexCompiled.MatchString(message.Content)
	if doesTzpreferenceRegexMatch {
		handleTzpreferenceRegexMatch(&eventState, tzpreferenceRegexCompiled.FindStringSubmatch(message.Content))
		return
	}

	const rmreminderRegex = `^!rmreminder (\d+)$`
	rmreminderRegexCompiled := regexp.MustCompile(rmreminderRegex)

	doesRmrreminderRegexMatch := rmreminderRegexCompiled.MatchString(message.Content)
	if doesRmrreminderRegexMatch {
		handleRmreminderRegexMatch(&eventState, rmreminderRegexCompiled.FindStringSubmatch(message.Content))
		return
	}

	const absoluteRemindmeRegex = `^!remindme on (\d{1,2})\.(\d{1,2})(?:\.(\d{4}))? at (\d{1,2})(?::(\d{1,2}))? (AM|PM) ?([a-zA-Z]+\/[a-zA-Z_]+)? (.+)`
	const relativeRemindmeRegex = `^!remindme in (\d{1,2}|an?) (minutes?|hours?|days?|weeks?|months?) (.+)`
	const recurringRemindmeRegex = `^!remindme every day at (\d{1,2})(?::(\d{1,2}))? (AM|PM) ?([a-zA-Z]+\/[a-zA-Z_]+)? (.+)`

	absoluteRemindmeRegexCompiled := regexp.MustCompile(absoluteRemindmeRegex)
	relativeRemindmeRegexCompiled := regexp.MustCompile(relativeRemindmeRegex)
	recurringRemindmeRegexCompiled := regexp.MustCompile(recurringRemindmeRegex)

	doesAbsoluteRegexMatch := absoluteRemindmeRegexCompiled.MatchString(message.Content)
	doesRelativeRegexMatch := relativeRemindmeRegexCompiled.MatchString(message.Content)
	doesRecurringRegexMatch := recurringRemindmeRegexCompiled.MatchString(message.Content)

	if strings.HasPrefix(message.Content, "!remindme") && !doesAbsoluteRegexMatch && !doesRelativeRegexMatch && !doesRecurringRegexMatch {
		eventState.reply(
			"Invalid `!remindme` syntax. Has to match either of these regexes:\n" +
				fmt.Sprintf("`%s`\n", absoluteRemindmeRegex) +
				fmt.Sprintf("`%s`\n\n", relativeRemindmeRegex) +
				"For example:\n" +
				"`!remindme on 23.12 at 12 PM America/New_York that Christmas is tomorrow`\n" +
				"`!remindme in 2 days to buy a gift for Aurora`\n\n" +
				"⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯\n\n" +
				"The timezone identifier (e.g. `America/New_York`), when specified, needs to match one of the identifiers from " +
				"the [IANA Time Zone Database](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones). " +
				"When not specified, first it checks whether you have a saved preference in the database, " +
				"and if not, defaults to `Europe/Warsaw`.\n\n" +
				"You can set your preference with:\n" +
				fmt.Sprintf("`%s`\n\n", tzpreferenceRegex) +
				"For example:\n" +
				"`!tzpreference Antarctica/South_Pole`",
		)
		return
	}

	if !doesAbsoluteRegexMatch && !doesRelativeRegexMatch && !doesRecurringRegexMatch {
		return
	}

	if doesAbsoluteRegexMatch {
		handleAbsoluteRegexMatch(&eventState, absoluteRemindmeRegexCompiled.FindStringSubmatch(message.Content))
		return
	}

	if doesRelativeRegexMatch {
		handleRelativeRegexMatch(&eventState, relativeRemindmeRegexCompiled.FindStringSubmatch(message.Content))
		return
	}

	handleRecurringRegexMatch(&eventState, recurringRemindmeRegexCompiled.FindStringSubmatch(message.Content))
}

func handleReminders(botSession *discordgo.Session, ticker *time.Ticker) {
	for currentTime := range ticker.C {
		if _, err := os.Stat("./reminders.db"); errors.Is(err, os.ErrNotExist) {
			log.Println("Database not bootstrapped yet, nothing to check.")
			continue
		}

		rows, err := dbHandle.Query("SELECT * FROM Reminders")
		if err != nil {
			log.Println("Error querying the rows when handling the reminders:", err)
		}

		rowsToDelete := []string{}
		rowsToUpdate := []rowToUpdate{}
		for rows.Next() {
			var (
				id        string
				who       string
				time      time.Time
				toRemind  string
				recurring bool
			)

			if err := rows.Scan(&id, &who, &time, &toRemind, &recurring); err != nil {
				log.Println("Error scanning the row:", err)
			}

			if currentTime.UTC().After(time) {
				_, err = botSession.ChannelMessageSend(remindersChannelId, fmt.Sprintf("<@%s>, reminding you %s.", who, toRemind))
				if err != nil {
					fmt.Println(err)
				}

				if recurring {
					newTime := time.AddDate(0, 0, 1)
					rowsToUpdate = append(rowsToUpdate, rowToUpdate{id: id, newTime: newTime, who: who})
				} else {
					rowsToDelete = append(rowsToDelete, id)
				}

			}
		}
		rows.Close()

		placeholders := make([]string, len(rowsToDelete))
		for i := range placeholders {
			placeholders[i] = "?"
		}

		_, err = dbHandle.Exec(
			fmt.Sprintf("DELETE FROM Reminders WHERE id IN (%s)", strings.Join(rowsToDelete, ",")),
		)
		if err != nil {
			log.Println("Error deleting the rows:", err)
		}

		for _, rowToUpdate := range rowsToUpdate {
			_, err := dbHandle.Exec(`
			UPDATE Reminders
			SET time=?
			WHERE id=?
			`, rowToUpdate.newTime, rowToUpdate.id)
			if err != nil {
				log.Println("Error updating the row:", err)
				botSession.ChannelMessageSend(remindersChannelId, fmt.Sprintf("<@%s>, couldn't update the recurring reminder. You might need to set it again.", rowToUpdate.who))
			}
		}
	}
}

func bootstrapDb() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "reminders.db")
	if err != nil {
		return db, err
	}

	row := db.QueryRow("PRAGMA user_version;")
	if row == nil {
		return db, errors.New("PRAGMA user_version not found")
	}
	var userVersion int
	if err = row.Scan(&userVersion); err != nil {
		return db, err
	}

	switch userVersion {
	// Base schema.
	case 0:
		tx, err := db.Begin()
		if err != nil {
			return db, err
		}
		defer tx.Rollback()

		_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS Reminders (
			id INTEGER NOT NULL PRIMARY KEY,
			who TEXT NOT NULL,
			time DATETIME NOT NULL,
			toRemind TEXT NOT NULL
		);`)
		if err != nil {
			return db, err
		}

		_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS TimezonePreferences (
			id INTEGER NOT NULL PRIMARY KEY,
			who TEXT NOT NULL,
			timezonePreference TEXT NOT NULL
		);`)
		if err != nil {
			return db, err
		}

		_, err = tx.Exec("PRAGMA user_version = 1;")
		if err != nil {
			return db, err
		}

		if err = tx.Commit(); err != nil {
			return db, err
		}

		fallthrough
	// Support recurring reminders.
	case 1:
		tx, err := db.Begin()
		if err != nil {
			return db, err
		}
		defer tx.Rollback()

		_, err = tx.Exec("ALTER TABLE Reminders ADD recurring INTEGER NOT NULL DEFAULT 0")
		if err != nil {
			return db, err
		}

		_, err = tx.Exec("PRAGMA user_version = 2;")
		if err != nil {
			return db, err
		}

		if err = tx.Commit(); err != nil {
			return db, err
		}
	}

	return db, nil
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
		dbHandle.Close()
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
