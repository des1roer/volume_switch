plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

android {
    namespace = "com.des1roer.volumeremote"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.des1roer.volumeremote"
        minSdk = 33
        targetSdk = 35
        versionCode = 7
        versionName = "1.6"

        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    signingConfigs {
        val keystore = providers.gradleProperty("volumeRemoteKeystore").orNull
        if (keystore != null) {
            create("release") {
                storeFile = file(keystore)
                storePassword = providers.gradleProperty("volumeRemoteStorePassword").get()
                keyAlias = providers.gradleProperty("volumeRemoteKeyAlias").get()
                keyPassword = providers.gradleProperty("volumeRemoteKeyPassword").get()
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
            signingConfig = signingConfigs.findByName("release") ?: signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        compose = true
    }
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.media3:media3-exoplayer:1.5.0")
    implementation("androidx.media3:media3-ui:1.5.0")
    implementation("org.jellyfin.media3:media3-ffmpeg-decoder:1.5.0+1")

    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20240303")
}
