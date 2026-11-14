package com.des1roer.volumeremote

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONException
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

data class Status(
    val mpcOnline: Boolean,
    val mpcError: String?,
    val file: String,
    val state: String,
    val positionMs: Long,
    val durationMs: Long,
    val remainingMs: Long,
    val mpcVolume: Int,
    val mpcMuted: Boolean,
    val volumePercent: Int?,
    val muted: Boolean,
    val volumeError: String?,
) {
    val playing: Boolean get() = state == "playing"
}

class ApiException(message: String) : Exception(message)

class RemoteClient(server: String) {
    private val baseUrl = normalizeServer(server)

    suspend fun status(): Status = request("GET", "/status")

    suspend fun playPause(): Status = request("POST", "/playpause")

    suspend fun next(): Status = request("POST", "/next")

    suspend fun prev(): Status = request("POST", "/prev")

    suspend fun volume(up: Boolean): Status = request("POST", if (up) "/volume/up" else "/volume/down")

    suspend fun mpcVolume(up: Boolean): Status = request("POST", if (up) "/mpc/volume/up" else "/mpc/volume/down")

    private suspend fun request(method: String, path: String): Status = withContext(Dispatchers.IO) {
        val conn = URL(baseUrl + path).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 2_000
            conn.readTimeout = 4_000
            val code = conn.responseCode
            val stream = if (code in 200..299) conn.inputStream else conn.errorStream
            val body = stream?.bufferedReader(Charsets.UTF_8)?.use { it.readText() }.orEmpty()
            if (code !in 200..299) {
                throw ApiException(errorMessage(body) ?: "HTTP $code")
            }
            parseStatus(body)
        } finally {
            conn.disconnect()
        }
    }
}

fun normalizeServer(server: String): String {
    val trimmed = server.trim().trimEnd('/')
    return if (trimmed.contains("://")) trimmed else "http://$trimmed"
}

fun errorMessage(body: String): String? = try {
    JSONObject(body).optString("error").ifEmpty { null }
} catch (e: JSONException) {
    body.trim().ifEmpty { null }
}

fun parseStatus(body: String): Status {
    val root = JSONObject(body)
    val mpc = root.getJSONObject("mpc")
    val volume = root.optJSONObject("volume")
    return Status(
        mpcOnline = mpc.optBoolean("online"),
        mpcError = mpc.optString("error").ifEmpty { null },
        file = mpc.optString("file"),
        state = mpc.optString("state", "none"),
        positionMs = mpc.optLong("position_ms"),
        durationMs = mpc.optLong("duration_ms"),
        remainingMs = mpc.optLong("remaining_ms"),
        mpcVolume = mpc.optInt("volume"),
        mpcMuted = mpc.optBoolean("muted"),
        volumePercent = volume?.optInt("percent"),
        muted = volume?.optBoolean("muted") ?: false,
        volumeError = root.optString("volume_error").ifEmpty { null },
    )
}

fun formatTime(ms: Long): String {
    val total = maxOf(ms, 0) / 1000
    val h = total / 3600
    val m = total % 3600 / 60
    val s = total % 60
    return if (h > 0) "%d:%02d:%02d".format(h, m, s) else "%02d:%02d".format(m, s)
}
