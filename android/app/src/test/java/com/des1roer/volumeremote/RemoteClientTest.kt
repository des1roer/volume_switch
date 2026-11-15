package com.des1roer.volumeremote

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class RemoteClientTest {
    @Test
    fun formatTime() {
        assertEquals("00:00", formatTime(-5_000))
        assertEquals("00:59", formatTime(59_999))
        assertEquals("01:01", formatTime(61_000))
        assertEquals("1:00:00", formatTime(3_600_000))
        assertEquals("2:37:03", formatTime(9_423_000))
    }

    @Test
    fun normalizeServer() {
        assertEquals("http://192.168.3.2:7778", normalizeServer(" 192.168.3.2:7778/ "))
        assertEquals("https://pc.local:7778", normalizeServer("https://pc.local:7778"))
    }

    @Test
    fun parseStatus() {
        val status = parseStatus(
            """
            {"version":"dev","mpc":{"online":true,"file":"movie.mkv","state":"playing","state_text":"Воспроизведение",
            "position_ms":61000,"duration_ms":3600000,"remaining_ms":3539000,"volume":70,"muted":false},
            "volume":{"percent":42,"muted":true}}
            """.trimIndent(),
        )
        assertTrue(status.mpcOnline)
        assertTrue(status.playing)
        assertEquals("movie.mkv", status.file)
        assertEquals(3_539_000, status.remainingMs)
        assertEquals(70, status.mpcVolume)
        assertFalse(status.mpcMuted)
        assertEquals(42, status.volumePercent)
        assertTrue(status.muted)
        assertNull(status.mpcError)
        assertNull(status.volumeError)
    }

    @Test
    fun seekedTo() {
        val status = Status(
            mpcOnline = true, mpcError = null, file = "movie.mkv", state = "playing",
            positionMs = 61_000, durationMs = 3_600_000, remainingMs = 3_539_000,
            mpcVolume = 70, mpcMuted = false, volumePercent = 42, muted = false, volumeError = null,
            receivedAt = 1_000,
        )
        val seeked = status.seekedTo(600_000, now = 5_000)
        assertEquals(600_000, seeked.positionMs)
        assertEquals(3_000_000, seeked.remainingMs)
        assertEquals(5_000, seeked.receivedAt)
        assertEquals(601_000, seeked.positionAt(6_000))
        assertEquals("movie.mkv", seeked.file)

        assertEquals(0, status.seekedTo(-10_000, now = 5_000).positionMs)
        assertEquals(3_600_000, status.seekedTo(9_999_999, now = 5_000).positionMs)
        assertEquals(0, status.seekedTo(9_999_999, now = 5_000).remainingMs)
    }

    @Test
    fun parseStatusOffline() {
        val status = parseStatus(
            """
            {"version":"dev","mpc":{"online":false,"error":"connection refused","file":"","state":"none",
            "state_text":"","position_ms":0,"duration_ms":0,"remaining_ms":0},"volume":null,
            "volume_error":"аудио не инициализировано"}
            """.trimIndent(),
        )
        assertFalse(status.mpcOnline)
        assertFalse(status.playing)
        assertEquals("connection refused", status.mpcError)
        assertNull(status.volumePercent)
        assertEquals("аудио не инициализировано", status.volumeError)
    }

    @Test
    fun sseParser() {
        val parser = SseParser()
        val lines = listOf(
            "data: {\"a\":1}",
            "",
            ": ping",
            "",
            "event: status",
            "data: line1",
            "data:line2",
            "",
            "",
        )
        val events = lines.mapNotNull { parser.line(it) }
        assertEquals(listOf("{\"a\":1}", "line1\nline2"), events)
    }

    @Test
    fun positionAt() {
        val playing = status(state = "playing", positionMs = 60_000, durationMs = 100_000, receivedAt = 1_000)
        assertEquals(60_000, playing.positionAt(1_000))
        assertEquals(70_000, playing.positionAt(11_000))
        assertEquals(30_000, playing.remainingAt(11_000))
        assertEquals(100_000, playing.positionAt(500_000))
        assertEquals(0, playing.remainingAt(500_000))
        assertEquals(60_000, playing.positionAt(0))

        val paused = playing.copy(state = "paused")
        assertEquals(60_000, paused.positionAt(11_000))
        assertEquals(40_000, paused.remainingAt(11_000))
    }

    @Test
    fun formatRemainingMinutes() {
        assertEquals("0 мин", formatRemainingMinutes(0))
        assertEquals("1 мин", formatRemainingMinutes(1))
        assertEquals("1 мин", formatRemainingMinutes(60_000))
        assertEquals("19 мин", formatRemainingMinutes(18 * 60_000 + 56_000))
        assertEquals("1 ч 05 мин", formatRemainingMinutes(65 * 60_000))
    }

    private fun status(state: String, positionMs: Long, durationMs: Long, receivedAt: Long) = Status(
        mpcOnline = true,
        mpcError = null,
        file = "a.mkv",
        state = state,
        positionMs = positionMs,
        durationMs = durationMs,
        remainingMs = durationMs - positionMs,
        mpcVolume = 0,
        mpcMuted = false,
        volumePercent = null,
        muted = false,
        volumeError = null,
        receivedAt = receivedAt,
    )

    @Test
    fun errorMessage() {
        assertEquals("MPC-HC не отвечает", errorMessage("""{"error":"MPC-HC не отвечает"}"""))
        assertEquals("Method Not Allowed", errorMessage("Method Not Allowed\n"))
        assertNull(errorMessage(""))
    }
}
