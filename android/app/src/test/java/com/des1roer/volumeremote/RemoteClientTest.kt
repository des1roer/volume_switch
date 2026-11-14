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
    fun errorMessage() {
        assertEquals("MPC-HC не отвечает", errorMessage("""{"error":"MPC-HC не отвечает"}"""))
        assertEquals("Method Not Allowed", errorMessage("Method Not Allowed\n"))
        assertNull(errorMessage(""))
    }
}
