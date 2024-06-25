# WAVStream

WAVStream is a WebSocket server designed to work with FreeSwitch's `mod_audio_stream` module. It enables real-time audio call recording, saving as WAV files, and automatic uploading to S3-compatible storage.

## Features

- Receive both text and audio data via WebSocket from the FreeSwitch `mod_audio_stream` module
- Create WAV files in real-time with filenames specified in the initial FreeSwitch connection command
- Automatic WAV header updating
- Automatic upload to S3-compatible storage upon recording completion
- Support for changing the recording file mid-call
- Concurrent handling of multiple recordings

## Installation

1. Clone the repository:
   ```
   git clone https://github.com/ynigun/wavstream.git
   cd wavstream
   ```

2. Install dependencies:
   ```
   go mod tidy
   ```

3. Create a `.env` file in the project directory and set the following variables:
   ```
   S3_ENDPOINT=your-s3-endpoint
   S3_ACCESS_KEY_ID=your-access-key
   S3_SECRET_ACCESS_KEY=your-secret-key
   S3_BUCKET_NAME=your-bucket-name
   S3_USE_SSL=true
   S3_PREFIX=audio/
   SERVER_PORT=8080
   ```

## Running

Start the server:

```
go run main.go
```

The server will start running on port 8080 by default.

## Usage with FreeSwitch

To start a recording with FreeSwitch, use the following command in the FreeSwitch CLI:

```
uuid_audio_stream <UUID> start ws://<SERVER_IP>:8080/ws mixed 8k rec_name_uuid.wav
```

Replace `<UUID>` with the UUID of the call, `<SERVER_IP>` with the IP address of the WAVStream server, and `rec_name_uuid.wav` with your desired filename for the recording.

To stop the recording and trigger the S3 upload:

```
uuid_audio_stream <UUID> stop
```

To change the recording file mid-call:

```
uuid_audio_stream <UUID> send_text rec_name_uuid_B.wav
```

This will close and upload the current recording to S3, then start a new recording with the specified filename.

## License

This project is distributed under the GNU General Public License v3.0. See the [LICENSE](LICENSE) file for more details.
