package com.des1roer.volumeremote

import android.app.Application
import android.content.Context
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

const val DEFAULT_SERVER = "192.168.3.2:7778"
private const val POLL_INTERVAL_MS = 1_000L

data class UiState(
    val server: String,
    val status: Status? = null,
    val error: String? = null,
)

class RemoteViewModel(app: Application) : AndroidViewModel(app) {
    private val prefs = app.getSharedPreferences("remote", Context.MODE_PRIVATE)

    var state by mutableStateOf(UiState(server = prefs.getString("server", null) ?: DEFAULT_SERVER))
        private set

    private val client get() = RemoteClient(state.server)

    suspend fun pollLoop() {
        while (true) {
            update { client.status() }
            delay(POLL_INTERVAL_MS)
        }
    }

    fun playPause() = launch { client.playPause() }

    fun next() = launch { client.next() }

    fun prev() = launch { client.prev() }

    fun volume(up: Boolean) = launch { client.volume(up) }

    fun setServer(server: String) {
        val value = server.trim()
        if (value.isEmpty() || value == state.server) return
        prefs.edit().putString("server", value).apply()
        state = UiState(server = value)
        launch { client.status() }
    }

    private fun launch(block: suspend () -> Status) {
        viewModelScope.launch { update(block) }
    }

    private suspend fun update(block: suspend () -> Status) {
        val server = state.server
        state = try {
            val status = block()
            if (server != state.server) return
            state.copy(status = status, error = null)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            if (server != state.server) return
            state.copy(error = e.message ?: e.javaClass.simpleName)
        }
    }
}
