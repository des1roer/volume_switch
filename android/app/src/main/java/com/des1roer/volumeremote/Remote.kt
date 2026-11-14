package com.des1roer.volumeremote

import android.content.Context
import android.content.SharedPreferences
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
private const val FAST_POLL_MS = 1_000L
private const val SLOW_POLL_MS = 5_000L
private const val SCREEN_OFF_POLL_MS = 30_000L

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

    private var users = 0
    private var uiVisible = false
    private var screenOn = true
    private var pollJob: Job? = null

    fun init(context: Context) {
        if (::prefs.isInitialized) return
        prefs = context.applicationContext.getSharedPreferences("remote", Context.MODE_PRIVATE)
        mutableState.value = UiState(server = prefs.getString("server", null) ?: DEFAULT_SERVER)
    }

    fun acquire(ui: Boolean = false) {
        users++
        if (ui) uiVisible = true
        if (pollJob == null || ui) restartPoll()
    }

    fun setScreenOn(on: Boolean) {
        if (on == screenOn) return
        screenOn = on
        if (on && pollJob != null) restartPoll()
    }

    private fun restartPoll() {
        pollJob?.cancel()
        pollJob = scope.launch { poll() }
    }

    fun release(ui: Boolean = false) {
        users = maxOf(users - 1, 0)
        if (ui) uiVisible = false
        if (users == 0) {
            pollJob?.cancel()
            pollJob = null
        }
    }

    fun playPause() = command { it.playPause() }

    fun next() = command { it.next() }

    fun prev() = command { it.prev() }

    fun volume(up: Boolean, step: Int? = null) = command { it.volume(up, step) }

    fun mpcVolume(up: Boolean) = command { it.mpcVolume(up) }

    fun setServer(server: String) {
        val value = server.trim()
        if (value.isEmpty() || value == mutableState.value.server) return
        prefs.edit().putString("server", value).apply()
        mutableState.value = UiState(server = value)
        command { it.status() }
    }

    private suspend fun poll() {
        while (true) {
            update { client().status() }
            delay(pollInterval(screenOn, uiVisible, mutableState.value.status?.playing == true))
        }
    }

    private fun command(block: suspend (RemoteClient) -> Status) {
        scope.launch { update { block(client()) } }
    }

    private fun client() = RemoteClient(mutableState.value.server)

    private suspend fun update(block: suspend () -> Status) {
        val server = mutableState.value.server
        try {
            val status = block()
            if (server == mutableState.value.server) {
                mutableState.value = mutableState.value.copy(status = status, error = null)
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            if (server == mutableState.value.server) {
                mutableState.value = mutableState.value.copy(error = e.message ?: e.javaClass.simpleName)
            }
        }
    }
}

fun pollInterval(screenOn: Boolean, uiVisible: Boolean, playing: Boolean): Long = when {
    !screenOn -> SCREEN_OFF_POLL_MS
    uiVisible || playing -> FAST_POLL_MS
    else -> SLOW_POLL_MS
}
