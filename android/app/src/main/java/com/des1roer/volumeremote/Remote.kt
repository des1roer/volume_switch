package com.des1roer.volumeremote

import android.content.Context
import android.content.SharedPreferences
import android.os.SystemClock
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch

const val DEFAULT_SERVER = "192.168.3.2:7778"
private val RECONNECT_DELAYS_MS = longArrayOf(1_000, 2_000, 5_000, 10_000)
private const val TABLET_MIN_WIDTH_DP = 600

data class UiState(
    val server: String,
    val status: Status? = null,
    val error: String? = null,
)

object Remote {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private lateinit var prefs: SharedPreferences

    private val mutableState = MutableStateFlow(UiState(server = DEFAULT_SERVER))
    val state: StateFlow<UiState> = mutableState

    private val mutableVolumeKeysToPc = MutableStateFlow(true)
    val volumeKeysToPc: StateFlow<Boolean> = mutableVolumeKeysToPc

    private var users = 0
    private var streamJob: Job? = null

    fun init(context: Context) {
        if (::prefs.isInitialized) return
        prefs = context.applicationContext.getSharedPreferences("remote", Context.MODE_PRIVATE)
        mutableState.value = UiState(server = prefs.getString("server", null) ?: DEFAULT_SERVER)
        val tablet = context.resources.configuration.smallestScreenWidthDp >= TABLET_MIN_WIDTH_DP
        mutableVolumeKeysToPc.value = prefs.getBoolean("volumeKeysToPc", !tablet)
    }

    fun setVolumeKeysToPc(value: Boolean) {
        prefs.edit().putBoolean("volumeKeysToPc", value).apply()
        mutableVolumeKeysToPc.value = value
    }

    fun acquire(ui: Boolean = false) {
        users++
        if (streamJob == null || ui) reconnect()
    }

    fun release() {
        users = maxOf(users - 1, 0)
        if (users == 0) {
            streamJob?.cancel()
            streamJob = null
        }
    }

    fun reconnect() {
        if (users == 0) return
        streamJob?.cancel()
        streamJob = scope.launch { stream() }
    }

    fun playPause() = command { it.playPause() }

    fun next() = command { it.next() }

    fun prev() = command { it.prev() }

    fun toggleOverlay() = command { it.toggleOverlay() }

    fun seekTo(positionMs: Long) = seek(positionMs) { it.seekTo(positionMs) }

    fun seekBy(offsetMs: Long) {
        val status = mutableState.value.status ?: return
        seek(status.positionAt(SystemClock.elapsedRealtime()) + offsetMs) { it.seekBy(offsetMs) }
    }

    fun volume(up: Boolean, step: Int? = null) = command { it.volume(up, step) }

    fun mpcVolume(up: Boolean) = command { it.mpcVolume(up) }

    fun setServer(server: String) {
        val value = server.trim()
        if (value.isEmpty() || value == mutableState.value.server) return
        prefs.edit().putString("server", value).apply()
        mutableState.value = UiState(server = value)
        reconnect()
    }

    private suspend fun stream() {
        var attempt = 0
        while (true) {
            val server = mutableState.value.server
            try {
                RemoteClient(server).events().collect { status ->
                    attempt = 0
                    apply(server, status)
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                fail(server, e)
            }
            delay(RECONNECT_DELAYS_MS[minOf(attempt, RECONNECT_DELAYS_MS.lastIndex)])
            attempt++
        }
    }

    private fun seek(targetMs: Long, block: suspend (RemoteClient) -> Status) {
        val ui = mutableState.value
        val status = ui.status ?: return
        mutableState.value = ui.copy(status = status.seekedTo(targetMs, SystemClock.elapsedRealtime()))
        command(onFail = ::reconnect, block = block)
    }

    private fun command(onFail: () -> Unit = {}, block: suspend (RemoteClient) -> Status) {
        scope.launch {
            val server = mutableState.value.server
            try {
                apply(server, block(RemoteClient(server)))
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                fail(server, e)
                onFail()
            }
        }
    }

    private fun apply(server: String, status: Status) {
        if (server != mutableState.value.server) return
        mutableState.value = mutableState.value.copy(
            status = status.copy(receivedAt = SystemClock.elapsedRealtime()),
            error = null,
        )
    }

    private fun fail(server: String, e: Exception) {
        if (server != mutableState.value.server) return
        mutableState.value = mutableState.value.copy(error = e.message ?: e.javaClass.simpleName)
    }
}
