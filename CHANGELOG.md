# Changelog

All notable changes to this project are documented here, in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format.

## [Unreleased]

## [0.1.1] - 2026-10-31

### Fixed

- The bot keeps every operator's room when the hub's superuser token expires, a day after start: it signs in again and acts on nothing until the hub takes it.

## [0.1.0] - 2026-10-29

### Added

- A bot that gives each operator a direct Matrix room and writes its alert URL into their Beszel notification settings, keeping the URL's token current.
- Operators stop the alerts by removing the URL in Beszel or leaving the room, and start them again by replying subscribe, after inviting the bot if they left.
- A Beszel user who is no longer an operator is deleted, with their alerts and settings, and the bot leaves their room.
