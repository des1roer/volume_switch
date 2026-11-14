package com.des1roer.volumeremote

import android.app.Application

class VolumeRemoteApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Remote.init(this)
    }
}
