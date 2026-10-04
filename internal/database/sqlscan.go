package database

import (
	"errors"
	"strings"
)

type sqlStatement struct {
	keywords []string
}

func scanSQLStatements(sql []byte) ([]sqlStatement, error) {
	var statements []sqlStatement
	current := sqlStatement{}
	active := false

	finishStatement := func() {
		if active {
			statements = append(statements, current)
		}
		current = sqlStatement{}
		active = false
	}

	for i := 0; i < len(sql); {
		switch {
		case isSQLSpace(sql[i]):
			i++
		case sql[i] == ';':
			finishStatement()
			i++
		case i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-':
			i += 2
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			var err error
			i, err = scanBlockComment(sql, i+2)
			if err != nil {
				return nil, err
			}
		case sql[i] == '\'':
			active = true
			var err error
			i, err = scanSingleQuoted(sql, i+1, false)
			if err != nil {
				return nil, err
			}
		case (sql[i] == 'e' || sql[i] == 'E') && i+1 < len(sql) && sql[i+1] == '\'' &&
			(i == 0 || !isSQLIdentifierPart(sql[i-1])):
			active = true
			var err error
			i, err = scanSingleQuoted(sql, i+2, true)
			if err != nil {
				return nil, err
			}
		case sql[i] == '"':
			active = true
			var err error
			i, err = scanDoubleQuoted(sql, i+1)
			if err != nil {
				return nil, err
			}
		case sql[i] == '$' && (i == 0 || !isSQLIdentifierPart(sql[i-1])):
			delimiter, afterDelimiter := dollarQuoteDelimiter(sql, i)
			if delimiter == "" {
				active = true
				i++
				continue
			}
			active = true
			end := strings.Index(string(sql[afterDelimiter:]), delimiter)
			if end < 0 {
				return nil, errors.New("unterminated dollar-quoted string in migration")
			}
			i = afterDelimiter + end + len(delimiter)
		case isSQLIdentifierStart(sql[i]):
			start := i
			for i++; i < len(sql) && isSQLIdentifierPart(sql[i]); i++ {
			}
			active = true
			if len(current.keywords) < 5 {
				current.keywords = append(current.keywords, strings.ToUpper(string(sql[start:i])))
			}
		default:
			active = true
			i++
		}
	}
	finishStatement()
	return statements, nil
}

func scanBlockComment(sql []byte, i int) (int, error) {
	depth := 1
	for i < len(sql) {
		switch {
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			depth++
			i += 2
		case i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i, nil
			}
		default:
			i++
		}
	}
	return 0, errors.New("unterminated block comment in migration")
}

func scanSingleQuoted(sql []byte, i int, escape bool) (int, error) {
	for i < len(sql) {
		if escape && sql[i] == '\\' {
			if i+1 >= len(sql) {
				return 0, errors.New("unterminated escape string in migration")
			}
			i += 2
			continue
		}
		if sql[i] != '\'' {
			i++
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '\'' {
			i += 2
			continue
		}
		return i + 1, nil
	}
	return 0, errors.New("unterminated string in migration")
}

func scanDoubleQuoted(sql []byte, i int) (int, error) {
	for i < len(sql) {
		if sql[i] != '"' {
			i++
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '"' {
			i += 2
			continue
		}
		return i + 1, nil
	}
	return 0, errors.New("unterminated quoted identifier in migration")
}

func dollarQuoteDelimiter(sql []byte, start int) (string, int) {
	i := start + 1
	if i < len(sql) && sql[i] == '$' {
		return "$$", i + 1
	}
	if i >= len(sql) || !isSQLIdentifierStart(sql[i]) {
		return "", start
	}
	for i++; i < len(sql) && isDollarTagPart(sql[i]); i++ {
	}
	if i >= len(sql) || sql[i] != '$' {
		return "", start
	}
	return string(sql[start : i+1]), i + 1
}

func isDollarTagPart(value byte) bool {
	return isSQLIdentifierStart(value) || value >= '0' && value <= '9'
}

func isTransactionControl(statement sqlStatement) bool {
	if len(statement.keywords) == 0 {
		return false
	}
	switch statement.keywords[0] {
	case "BEGIN", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE":
		return true
	case "START", "PREPARE":
		return len(statement.keywords) > 1 && statement.keywords[1] == "TRANSACTION"
	case "SET":
		if len(statement.keywords) > 1 && statement.keywords[1] == "TRANSACTION" {
			return true
		}
		return len(statement.keywords) >= 5 && statement.keywords[1] == "SESSION" &&
			statement.keywords[2] == "CHARACTERISTICS" && statement.keywords[3] == "AS" &&
			statement.keywords[4] == "TRANSACTION"
	default:
		return false
	}
}

func isSQLSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isSQLIdentifierStart(value byte) bool {
	// PostgreSQL permits non-ASCII letters in unquoted identifiers. Treat every
	// UTF-8 byte conservatively as an identifier byte so a dollar quote can
	// never be opened from the middle of such an identifier.
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= 0x80
}

func isSQLIdentifierPart(value byte) bool {
	return isSQLIdentifierStart(value) || value >= '0' && value <= '9' || value == '$'
}
